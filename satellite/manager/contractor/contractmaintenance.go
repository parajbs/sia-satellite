package contractor

// contractmaintenance.go handles forming and renewing contracts for the
// contractor. This includes deciding when new contracts need to be formed, when
// contracts need to be renewed, and if contracts need to be blacklisted.

import (
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"time"

	"github.com/mike76-dev/sia-satellite/modules"
	"github.com/mike76-dev/sia-satellite/satellite/manager/proto"

	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/NebulousLabs/fastrand"

	"go.sia.tech/siad/build"
	smodules "go.sia.tech/siad/modules"
	"go.sia.tech/siad/types"
)

// MaxCriticalRenewFailThreshold is the maximum number of contracts failing to renew as
// fraction of the total hosts in the allowance before renew alerts are made
// critical.
const MaxCriticalRenewFailThreshold = 0.2

var (
	// ErrInsufficientAllowance indicates that the renter's allowance is less
	// than the amount necessary to store at least one sector
	ErrInsufficientAllowance = errors.New("allowance is not large enough to cover fees of contract creation")
	errTooExpensive          = errors.New("host price was too high")

	// errContractEnded is the error returned when the contract has already ended
	errContractEnded = errors.New("contract has already ended")

	// errContractNotGFR is used to indicate that a contract renewal failed
	// because the contract was marked !GFR.
	errContractNotGFR = errors.New("contract is not GoodForRenew")

	// errHostBlocked is the error returned when the host is blocked
	errHostBlocked = errors.New("host is blocked")
)

type (
	// fileContractRenewal is an instruction to renew a file contract.
	fileContractRenewal struct {
		id              types.FileContractID
		amount          types.Currency
		hostPubKey      types.SiaPublicKey
		renterPubKey    types.SiaPublicKey
	}
)

// callNotifyDoubleSpend is used by the watchdog to alert the contractor
// whenever a monitored file contract input is double-spent. This function
// marks down the host score, and marks the contract as !GoodForRenew and
// !GoodForUpload.
func (c *Contractor) callNotifyDoubleSpend(fcID types.FileContractID, blockHeight types.BlockHeight) {
	c.log.Println("Watchdog found a double-spend: ", fcID, blockHeight)

	// Mark the contract as double-spent. This will cause the contract to be
	// excluded in period spending.
	c.mu.Lock()
	c.doubleSpentContracts[fcID] = blockHeight
	c.mu.Unlock()

	err := c.MarkContractBad(fcID)
	if err != nil {
		c.log.Println("callNotifyDoubleSpend error in MarkContractBad", err)
	}
}

// managedCheckForDuplicates checks for static contracts that have the same host
// key and the same renter key and moves the older one to old contracts.
func (c *Contractor) managedCheckForDuplicates() {
	// Build map for comparison.
	pubkeys := make(map[string]types.FileContractID)
	var newContract, oldContract modules.RenterContract
	for _, contract := range c.staticContracts.ViewAll() {
		key := contract.RenterPublicKey.String() + contract.HostPublicKey.String()
		id, exists := pubkeys[key]
		if !exists {
			pubkeys[key] = contract.ID
			continue
		}

		// Duplicate contract found, determine older contract to delete.
		if rc, ok := c.staticContracts.View(id); ok {
			if rc.StartHeight >= contract.StartHeight {
				newContract, oldContract = rc, contract
			} else {
				newContract, oldContract = contract, rc
			}
			c.log.Printf("Duplicate contract found. New contract is %x and old contract is %v\n", newContract.ID, oldContract.ID)

			// Get FileContract.
			oldSC, ok := c.staticContracts.Acquire(oldContract.ID)
			if !ok {
				// Update map.
				pubkeys[key] = newContract.ID
				continue
			}

			// Link the contracts to each other and then store the old contract
			// in the record of historic contracts.
			//
			// Note: This means that if there are multiple duplicates, say 3
			// contracts that all share the same host, then the ordering may not
			// be perfect. If in reality the renewal order was A<->B<->C, it's
			// possible for the contractor to end up with A->C and B<->C in the
			// mapping.
			c.mu.Lock()
			c.renewedFrom[newContract.ID] = oldContract.ID
			c.renewedTo[oldContract.ID] = newContract.ID
			c.oldContracts[oldContract.ID] = oldSC.Metadata()

			// Save the contractor and delete the contract.
			//
			// TODO: Ideally these two things would happen atomically, but I'm
			// not completely certain that's feasible with our current
			// architecture.
			//
			// TODO: This should revert the in-memory state in the event of an
			// error and continue.
			err := c.save()
			if err != nil {
				c.log.Println("Failed to save the contractor after updating renewed maps.")
			}
			c.mu.Unlock()
			c.staticContracts.Delete(oldSC)

			// Update the pubkeys map to contain the newest contract id.
			pubkeys[key] = newContract.ID
		}
	}
}

// managedEstimateRenewFundingRequirements estimates the amount of money that a
// contract is going to need in the next billing cycle by looking at how much
// storage is in the contract and what the historic usage pattern of the
// contract has been.
func (c *Contractor) managedEstimateRenewFundingRequirements(contract modules.RenterContract, blockHeight types.BlockHeight, allowance smodules.Allowance) (types.Currency, error) {
	// Fetch the host pricing to use in the estimate.
	host, exists, err := c.hdb.Host(contract.HostPublicKey)
	if err != nil {
		return types.ZeroCurrency, errors.AddContext(err, "error getting host from hostdb:")
	}
	if !exists {
		return types.ZeroCurrency, errors.New("could not find host in hostdb")
	}
	if host.Filtered {
		return types.ZeroCurrency, errHostBlocked
	}

	// Fetch the renter.
	c.mu.RLock()
	renter, exists := c.renters[contract.RenterPublicKey.String()]
	c.mu.RUnlock()
	if !exists {
		return types.ZeroCurrency, ErrRenterNotFound
	}

	// Estimate the amount of money that's going to be needed for existing
	// storage.
	dataStored := contract.Transaction.FileContractRevisions[0].NewFileSize
	storageCost := types.NewCurrency64(dataStored).Mul64(uint64(allowance.Period)).Mul(host.StoragePrice)

	// For the spending estimates, we're going to need to know the amount of
	// money that was spent on upload and download by this contract line in this
	// period. That's going to require iterating over the renew history of the
	// contract to get all the spending across any refreshes that occurred this
	// period.
	prevUploadSpending := contract.UploadSpending
	prevDownloadSpending := contract.DownloadSpending
	prevFundAccountSpending := contract.FundAccountSpending
	prevMaintenanceSpending := contract.MaintenanceSpending
	c.mu.Lock()
	currentID := contract.ID
	for i := 0; i < 10e3; i++ { // Prevent an infinite loop if there's an [impossible] contract cycle.
		// If there is no previous contract, nothing to do.
		var exists bool
		currentID, exists = c.renewedFrom[currentID]
		if !exists {
			break
		}

		// If the contract is not in oldContracts, that's probably a bug, but
		// nothing to do otherwise.
		currentContract, exists := c.oldContracts[currentID]
		if !exists {
			c.log.Println("WARN: A known previous contract is not found in c.oldContracts")
			break
		}

		// If the contract did not start in the current period, then it is not
		// relevant, and none of the previous contracts will be relevant either.
		if currentContract.StartHeight < renter.CurrentPeriod {
			break
		}

		// Add the historical spending metrics.
		prevUploadSpending = prevUploadSpending.Add(currentContract.UploadSpending)
		prevDownloadSpending = prevDownloadSpending.Add(currentContract.DownloadSpending)
		prevFundAccountSpending = prevFundAccountSpending.Add(currentContract.FundAccountSpending)
		prevMaintenanceSpending = prevMaintenanceSpending.Add(currentContract.MaintenanceSpending)
	}
	c.mu.Unlock()

	// Estimate the amount of money that's going to be needed for new storage
	// based on the amount of new storage added in the previous period. Account
	// for both the storage price as well as the upload price.
	prevUploadDataEstimate := prevUploadSpending
	if !host.UploadBandwidthPrice.IsZero() {
		// TODO: Because the host upload bandwidth price can change, this is not
		// the best way to estimate the amount of data that was uploaded to this
		// contract. Better would be to look at the amount of data stored in the
		// contract from the previous cycle and use that to determine how much
		// total data.
		prevUploadDataEstimate = prevUploadDataEstimate.Div(host.UploadBandwidthPrice)
	}
	// Sanity check - the host may have changed prices, make sure we aren't
	// assuming an unreasonable amount of data.
	if types.NewCurrency64(dataStored).Cmp(prevUploadDataEstimate) < 0 {
		prevUploadDataEstimate = types.NewCurrency64(dataStored)
	}
	// The estimated cost for new upload spending is the previous upload
	// bandwidth plus the implied storage cost for all of the new data.
	newUploadsCost := prevUploadSpending.Add(prevUploadDataEstimate.Mul64(uint64(allowance.Period)).Mul(host.StoragePrice))

	// The download cost is assumed to be the same. Even if the user is
	// uploading more data, the expectation is that the download amounts will be
	// relatively constant. Add in the contract price as well.
	newDownloadsCost := prevDownloadSpending

	// The estimated cost for funding ephemeral accounts and performing RHP3
	// maintenance such as updating price tables and syncing the ephemeral
	// account balance is expected to remain identical.
	newFundAccountCost := prevFundAccountSpending
	newMaintenanceCost := prevMaintenanceSpending.Sum()

	contractPrice := host.ContractPrice

	// Aggregate all estimates so far to compute the estimated siafunds fees.
	// The transaction fees are not included in the siafunds estimate because
	// users are not charged siafund fees on money that doesn't go into the file
	// contract (and the transaction fee goes to the miners, not the file
	// contract).
	beforeSiafundFeesEstimate := storageCost.Add(newUploadsCost).Add(newDownloadsCost).Add(newFundAccountCost).Add(newMaintenanceCost).Add(contractPrice)
	afterSiafundFeesEstimate := types.Tax(blockHeight, beforeSiafundFeesEstimate).Add(beforeSiafundFeesEstimate)

	// Get an estimate for how much money we will be charged before going into
	// the transaction pool.
	_, maxTxnFee := c.tpool.FeeEstimation()
	txnFees := maxTxnFee.Mul64(smodules.EstimatedFileContractTransactionSetSize)

	// Add them all up and then return the estimate plus 33% for error margin
	// and just general volatility of usage pattern.
	estimatedCost := afterSiafundFeesEstimate.Add(txnFees)
	estimatedCost = estimatedCost.Add(estimatedCost.Div64(3))

	// Check for a sane minimum. The contractor should not be forming contracts
	// with less than 'fileContractMinimumFunding / (num contracts)' of the
	// value of the allowance.
	minimum := allowance.Funds.MulFloat(fileContractMinimumFunding).Div64(allowance.Hosts)
	if estimatedCost.Cmp(minimum) < 0 {
		estimatedCost = minimum
	}
	return estimatedCost, nil
}

// callInterruptContractMaintenance will issue an interrupt signal to any
// running maintenance, stopping that maintenance. If there are multiple threads
// running maintenance, they will all be stopped.
func (c *Contractor) callInterruptContractMaintenance() {
	// Spin up a thread to grab the maintenance lock. Signal that the lock was
	// acquired after the lock is acquired.
	gotLock := make(chan struct{})
	go func() {
		c.maintenanceLock.Lock()
		close(gotLock)
		c.maintenanceLock.Unlock()
	}()

	// There may be multiple threads contending for the maintenance lock. Issue
	// interrupts repeatedly until we get a signal that the maintenance lock has
	// been acquired.
	for {
		select {
		case <-gotLock:
			return
		case c.interruptMaintenance <- struct{}{}:
			c.log.Println("Signal sent to interrupt contract maintenance")
		}
	}
}

// managedFindMinAllowedHostScores uses a set of random hosts from the hostdb to
// calculate minimum acceptable score for a host to be marked GFR and GFU.
func (c *Contractor) managedFindMinAllowedHostScores(rpk types.SiaPublicKey) (types.Currency, types.Currency, error) {
	// Check if we know this renter.
	c.mu.RLock()
	renter, exists := c.renters[rpk.String()]
	c.mu.RUnlock()
	if !exists {
		return types.Currency{}, types.Currency{}, ErrRenterNotFound
	}
	
	// Pull a new set of hosts from the hostdb that could be used as a new set
	// to match the allowance. The lowest scoring host of these new hosts will
	// be used as a baseline for determining whether our existing contracts are
	// worthwhile.
	hostCount := int(renter.Allowance.Hosts)
	hosts, err := c.hdb.RandomHostsWithLimits(hostCount + randomHostsBufferForScore, nil, nil, renter.Allowance)
	if err != nil {
		return types.Currency{}, types.Currency{}, err
	}

	if len(hosts) == 0 {
		return types.Currency{}, types.Currency{}, errors.New("No hosts returned in RandomHosts")
	}

	// Find the minimum score that a host is allowed to have to be considered
	// good for upload.
	var minScoreGFR, minScoreGFU types.Currency
	sb, err := c.hdb.ScoreBreakdown(hosts[0])
	if err != nil {
		return types.Currency{}, types.Currency{}, err
	}

	lowestScore := sb.Score
	for i := 1; i < len(hosts); i++ {
		score, err := c.hdb.ScoreBreakdown(hosts[i])
		if err != nil {
			return types.Currency{}, types.Currency{}, err
		}
		if score.Score.Cmp(lowestScore) < 0 {
			lowestScore = score.Score
		}
	}
	// Set the minimum acceptable score to a factor of the lowest score.
	minScoreGFR = lowestScore.Div(scoreLeewayGoodForRenew)
	minScoreGFU = lowestScore.Div(scoreLeewayGoodForUpload)

	return minScoreGFR, minScoreGFU, nil
}

// managedNewContract negotiates an initial file contract with the specified
// host, saves it, and returns it.
func (c *Contractor) managedNewContract(rpk types.SiaPublicKey, host smodules.HostDBEntry, contractFunding types.Currency, endHeight types.BlockHeight) (_ types.Currency, _ modules.RenterContract, err error) {
	// Check if we know this renter.
	c.mu.RLock()
	renter, exists := c.renters[rpk.String()]
	c.mu.RUnlock()
	if !exists {
		return types.ZeroCurrency, modules.RenterContract{}, ErrRenterNotFound
	}

	// Reject hosts that are too expensive.
	if host.StoragePrice.Cmp(maxStoragePrice) > 0 {
		return types.ZeroCurrency, modules.RenterContract{}, errTooExpensive
	}
	// Determine if host settings align with allowance period.
	c.mu.Lock()
	if reflect.DeepEqual(renter.Allowance, smodules.Allowance{}) {
		c.mu.Unlock()
		return types.ZeroCurrency, modules.RenterContract{}, errors.New("called managedNewContract but allowance wasn't set")
	}
	hostSettings := host.HostExternalSettings
	period := renter.Allowance.Period
	c.mu.Unlock()

	if host.MaxDuration < period {
		err := errors.New("unable to form contract with host due to insufficient MaxDuration of host")
		return types.ZeroCurrency, modules.RenterContract{}, err
	}
	// Cap host.MaxCollateral.
	if host.MaxCollateral.Cmp(maxCollateral) > 0 {
		host.MaxCollateral = maxCollateral
	}

	// Check for price gouging.
	err = checkFormContractGouging(renter.Allowance, hostSettings)
	if err != nil {
		return types.ZeroCurrency, modules.RenterContract{}, errors.AddContext(err, "unable to form a contract due to price gouging detection")
	}

	// Get an address to use for negotiation.
	uc, err := c.wallet.NextAddress()
	if err != nil {
		return types.ZeroCurrency, modules.RenterContract{}, err
	}
	defer func() {
		if err != nil {
			err = errors.Compose(err, c.wallet.MarkAddressUnused(uc))
		}
	}()

	// Get the wallet seed.
	seed, _, err := c.wallet.PrimarySeed()
	if err != nil {
		return types.ZeroCurrency, modules.RenterContract{}, err
	}

	// Derive the renter seed and wipe it once we are done with it.
	renterSeed := modules.DeriveRenterSeed(seed, renter.Email)
	defer fastrand.Read(renterSeed[:])

	// Create contract params.
	c.mu.RLock()
	params := smodules.ContractParams{
		Allowance:     renter.Allowance,
		Host:          host,
		Funding:       contractFunding,
		StartHeight:   c.blockHeight,
		EndHeight:     endHeight,
		RefundAddress: uc.UnlockHash(),
		RenterSeed:    smodules.EphemeralRenterSeed(renterSeed), // The seed should not be mutated.
	}
	c.mu.RUnlock()

	// Wipe the renter seed once we are done using it.
	defer fastrand.Read(params.RenterSeed[:])

	// Create transaction builder and trigger contract formation.
	txnBuilder, err := c.wallet.StartTransaction()
	if err != nil {
		return types.ZeroCurrency, modules.RenterContract{}, err
	}

	contract, formationTxnSet, sweepTxn, sweepParents, err := c.staticContracts.FormContract(params, txnBuilder, c.tpool, c.hdb, c.tg.StopChan())
	if err != nil {
		txnBuilder.Drop()
		return types.ZeroCurrency, modules.RenterContract{}, err
	}

	monitorContractArgs := monitorContractArgs{
		false,
		contract.ID,
		contract.Transaction,
		formationTxnSet,
		sweepTxn,
		sweepParents,
		params.StartHeight,
	}
	err = c.staticWatchdog.callMonitorContract(monitorContractArgs)
	if err != nil {
		return types.ZeroCurrency, modules.RenterContract{}, err
	}

	// Add a mapping from the contract's id to the public keys of the host
	// and the renter.
	c.mu.Lock()
	_, exists = c.pubKeysToContractID[contract.RenterPublicKey.String() + contract.HostPublicKey.String()]
	if exists {
		c.mu.Unlock()
		txnBuilder.Drop()
		// We need to return a funding value because money was spent on this
		// host, even though the full process could not be completed.
		c.log.Println("WARN: Attempted to form a new contract with a host that this renter already has a contract with.")
		return contractFunding, modules.RenterContract{}, fmt.Errorf("%v already has a contract with host %v", contract.RenterPublicKey.String(), contract.HostPublicKey.String())
	}
	c.pubKeysToContractID[contract.RenterPublicKey.String() + contract.HostPublicKey.String()] = contract.ID
	c.mu.Unlock()

	contractValue := contract.RenterFunds
	c.log.Printf("Formed contract %v with %v for %v\n", contract.ID, host.NetAddress, contractValue.HumanString())

	// Update the hostdb to include the new contract.
	err = c.hdb.UpdateContracts(c.staticContracts.ViewAll())
	if err != nil {
		c.log.Println("Unable to update hostdb contracts:", err)
	}
	return contractFunding, contract, nil
}

// managedPruneRedundantAddressRange uses the hostdb to find hosts that
// violate the rules about address ranges and cancels them.
func (c *Contractor) managedPruneRedundantAddressRange() {
	// Get all contracts which are not canceled.
	allContracts := c.staticContracts.ViewAll()
	var contracts []modules.RenterContract
	for _, contract := range allContracts {
		if contract.Utility.Locked && !contract.Utility.GoodForRenew && !contract.Utility.GoodForUpload {
			// Contract is canceled.
			continue
		}
		contracts = append(contracts, contract)
	}

	// Get all the public keys and map them to contract ids.
	hosts := make(map[string]struct{})
	pks := make([]types.SiaPublicKey, 0, len(allContracts))
	cids := make(map[string][]types.FileContractID)
	var fcids []types.FileContractID
	var key string
	var exists bool
	for _, contract := range contracts {
		key = contract.HostPublicKey.String()
		if _, exists := hosts[key]; !exists {
			pks = append(pks, contract.HostPublicKey)
			hosts[key] = struct{}{}
		}
		fcids, exists = cids[key]
		if exists {
			cids[key] = append(fcids, contract.ID)
		} else {
			cids[key] = []types.FileContractID{contract.ID}
		}
	}

	// Let the hostdb filter out bad hosts and cancel contracts with those
	// hosts.
	badHosts, err := c.hdb.CheckForIPViolations(pks)
	if err != nil {
		c.log.Println("WARN: error checking for IP violations:", err)
		return
	}
	for _, host := range badHosts {
		// Multiple renters can have contracts with the same host, so we need
		// to iterate through those, too.
		for _, fcid := range cids[host.String()] {
			if err := c.managedCancelContract(fcid); err != nil {
				c.log.Println("WARN: unable to cancel contract in managedPruneRedundantAddressRange", err)
			}
		}
	}
}

// managedLimitGFUHosts caps the number of GFU hosts to allowance.Hosts.
func (c *Contractor) managedLimitGFUHosts() {
	c.mu.Lock()
	renters := c.renters
	c.mu.Unlock()
	// Get all GFU contracts and their score.
	type gfuContract struct {
		c     modules.RenterContract
		score types.Currency
	}
	var gfuContracts []gfuContract
	var key string
	hostScores := make(map[string]types.Currency)
	for _, contract := range c.Contracts() {
		if !contract.Utility.GoodForUpload {
			continue
		}
		key = contract.HostPublicKey.String()
		hostScore, exists := hostScores[key]
		if !exists {
			host, ok, err := c.hdb.Host(contract.HostPublicKey)
			if !ok || err != nil {
				c.log.Println("managedLimitGFUHosts was run after updating contract utility but found contract without host in hostdb that's GFU", contract.HostPublicKey)
				continue
			}
			score, err := c.hdb.ScoreBreakdown(host)
			if err != nil {
				c.log.Println("managedLimitGFUHosts: failed to get score breakdown for GFU host")
				continue
			}
			hostScores[key] = score.Score
			hostScore = score.Score
		}
		gfuContracts = append(gfuContracts, gfuContract{
			c:     contract,
			score: hostScore,
		})
	}
	// Sort gfuContracts by score.
	sort.Slice(gfuContracts, func(i, j int) bool {
		return gfuContracts[i].score.Cmp(gfuContracts[j].score) < 0
	})
	// Mark them bad for upload until we are below the expected number of hosts
	// for each renter.
	numHosts := make(map[string]uint64)
	for _, renter := range renters {
		numHosts[renter.PublicKey.String()] = renter.Allowance.Hosts
	}
	for _, contract := range gfuContracts {
		// Check if this renter has enough hosts already.
		key = contract.c.RenterPublicKey.String()
		if numHosts[key] > 0 {
			numHosts[key] = numHosts[key] - 1
			continue
		}
		sc, ok := c.staticContracts.Acquire(contract.c.ID)
		if !ok {
			c.log.Println("managedLimitGFUHosts: failed to acquire GFU contract")
			continue
		}
		u := sc.Utility()
		u.GoodForUpload = false
		err := c.managedUpdateContractUtility(sc, u)
		c.staticContracts.Return(sc)
		if err != nil {
			c.log.Println("managedLimitGFUHosts: failed to update GFU contract utility")
			continue
		}
	}
}

// staticCheckFormPaymentContractGouging will check whether the pricing from the
// host for forming a payment contract is too high to justify forming a contract
// with this host.
func staticCheckFormPaymentContractGouging(allowance smodules.Allowance, hostSettings smodules.HostExternalSettings) error {
	// Check whether the RPC base price is too high.
	if !allowance.MaxRPCPrice.IsZero() && allowance.MaxRPCPrice.Cmp(hostSettings.BaseRPCPrice) <= 0 {
		return errors.New("rpc base price of host is too high - extortion protection enabled")
	}
	// Check whether the form contract price is too high.
	if !allowance.MaxContractPrice.IsZero() && allowance.MaxContractPrice.Cmp(hostSettings.ContractPrice) <= 0 {
		return errors.New("contract price of host is too high - extortion protection enabled")
	}
	// Check whether the sector access price is too high.
	if !allowance.MaxSectorAccessPrice.IsZero() && allowance.MaxSectorAccessPrice.Cmp(hostSettings.SectorAccessPrice) <= 0 {
		return errors.New("sector access price of host is too high - extortion protection enabled")
	}
	return nil
}

// checkFormContractGouging will check whether the pricing for forming
// this contract triggers any price gouging warnings.
func checkFormContractGouging(allowance smodules.Allowance, hostSettings smodules.HostExternalSettings) error {
	// Check whether the RPC base price is too high.
	if !allowance.MaxRPCPrice.IsZero() && allowance.MaxRPCPrice.Cmp(hostSettings.BaseRPCPrice) < 0 {
		return errors.New("rpc base price of host is too high - price gouging protection enabled")
	}
	// Check whether the form contract price is too high.
	if !allowance.MaxContractPrice.IsZero() && allowance.MaxContractPrice.Cmp(hostSettings.ContractPrice) < 0 {
		return errors.New("contract price of host is too high - price gouging protection enabled")
	}

	return nil
}

// managedRenew negotiates a new contract for data already stored with a host.
// It returns the new contract. This is a blocking call that performs network
// I/O.
func (c *Contractor) managedRenew(id types.FileContractID, rpk types.SiaPublicKey, hpk types.SiaPublicKey, contractFunding types.Currency, newEndHeight types.BlockHeight, hostSettings smodules.HostExternalSettings) (_ modules.RenterContract, err error) {
	// Check if we know this renter.
	c.mu.RLock()
	renter, exists := c.renters[rpk.String()]
	c.mu.RUnlock()
	if !exists {
		return modules.RenterContract{}, ErrRenterNotFound
	}

	// Fetch the host associated with this contract.
	host, ok, err := c.hdb.Host(hpk)
	if err != nil {
		return modules.RenterContract{}, errors.AddContext(err, "error getting host from hostdb:")
	}
	// Use the most recent hostSettings, along with the host db entry.
	host.HostExternalSettings = hostSettings

	if reflect.DeepEqual(renter.Allowance, smodules.Allowance{}) {
		return modules.RenterContract{}, errors.New("called managedRenew but allowance isn't set")
	}
	period := renter.Allowance.Period

	if !ok {
		return modules.RenterContract{}, errHostNotFound
	} else if host.Filtered {
		return modules.RenterContract{}, errHostBlocked
	} else if host.StoragePrice.Cmp(maxStoragePrice) > 0 {
		return modules.RenterContract{}, errTooExpensive
	} else if host.MaxDuration < period {
		return modules.RenterContract{}, errors.New("insufficient MaxDuration of host")
	}

	// Cap host.MaxCollateral.
	if host.MaxCollateral.Cmp(maxCollateral) > 0 {
		host.MaxCollateral = maxCollateral
	}

	// Check for price gouging on the renewal.
	err = checkFormContractGouging(renter.Allowance, host.HostExternalSettings)
	if err != nil {
		return modules.RenterContract{}, errors.AddContext(err, "unable to renew - price gouging protection enabled")
	}

	// Get an address to use for negotiation.
	uc, err := c.wallet.NextAddress()
	if err != nil {
		return modules.RenterContract{}, err
	}
	defer func() {
		if err != nil {
			err = errors.Compose(err, c.wallet.MarkAddressUnused(uc))
		}
	}()

	// Get the wallet seed.
	seed, _, err := c.wallet.PrimarySeed()
	if err != nil {
		return modules.RenterContract{}, err
	}

	// Derive the renter seed and wipe it after we are done with it.
	renterSeed := modules.DeriveRenterSeed(seed, renter.Email)
	defer fastrand.Read(renterSeed[:])

	// Create contract params.
	c.mu.RLock()
	params := smodules.ContractParams{
		Allowance:     renter.Allowance,
		Host:          host,
		Funding:       contractFunding,
		StartHeight:   c.blockHeight,
		EndHeight:     newEndHeight,
		RefundAddress: uc.UnlockHash(),
		RenterSeed:    smodules.EphemeralRenterSeed(renterSeed), // The seed should not be mutated.
	}
	c.mu.RUnlock()

	// Wipe the renter seed once we are done using it.
	defer fastrand.Read(params.RenterSeed[:])

	// Create a transaction builder with the correct amount of funding for the renewal.
	txnBuilder, err := c.wallet.StartTransaction()
	if err != nil {
		return modules.RenterContract{}, err
	}
	err = txnBuilder.FundSiacoins(params.Funding)
	if err != nil {
		txnBuilder.Drop() // Return unused outputs to wallet.
		return modules.RenterContract{}, err
	}
	// Add an output that sends all fund back to the refundAddress.
	// Note that in order to send this transaction, a miner fee will have to be subtracted.
	output := types.SiacoinOutput{
		Value:      params.Funding,
		UnlockHash: params.RefundAddress,
	}
	sweepTxn, sweepParents := txnBuilder.Sweep(output)

	var newContract modules.RenterContract
	var formationTxnSet []types.Transaction

	oldContract, ok := c.staticContracts.Acquire(id)
	if !ok {
		return modules.RenterContract{}, errContractNotFound
	}
	if !oldContract.Utility().GoodForRenew {
		return modules.RenterContract{}, errContractNotGFR
	}
	newContract, formationTxnSet, err = c.staticContracts.Renew(oldContract, params, txnBuilder, c.tpool, c.hdb, c.tg.StopChan())
	c.staticContracts.Return(oldContract)
	if err != nil {
		txnBuilder.Drop() // Return unused outputs to wallet.
		return modules.RenterContract{}, err
	}

	monitorContractArgs := monitorContractArgs{
		false,
		newContract.ID,
		newContract.Transaction,
		formationTxnSet,
		sweepTxn,
		sweepParents,
		params.StartHeight,
	}
	err = c.staticWatchdog.callMonitorContract(monitorContractArgs)
	if err != nil {
		return modules.RenterContract{}, err
	}

	// Add a mapping from the contract's id to the public keys of the renter
	// and the host. This will destroy the previous mapping from pubKey to
	// contract id but other modules are only interested in the most recent
	// contract anyway.
	c.mu.Lock()
	c.pubKeysToContractID[newContract.RenterPublicKey.String() + newContract.HostPublicKey.String()] = newContract.ID
	c.mu.Unlock()

	// Update the hostdb to include the new contract.
	err = c.hdb.UpdateContracts(c.staticContracts.ViewAll())
	if err != nil {
		c.log.Println("Unable to update hostdb contracts:", err)
	}

	return newContract, nil
}

// managedRenewContract will use the renew instructions to renew a contract,
// returning the amount of money that was put into the contract for renewal.
func (c *Contractor) managedRenewContract(renewInstructions fileContractRenewal, blockHeight, endHeight types.BlockHeight) (fundsSpent types.Currency, newContract modules.RenterContract, err error) {
	// Check if we know this renter.
	key := renewInstructions.renterPubKey.String()
	c.mu.RLock()
	renter, exists := c.renters[key]
	c.mu.RUnlock()
	if !exists {
		return types.ZeroCurrency, newContract, ErrRenterNotFound
	}

	// Pull the variables out of the renewal.
	id := renewInstructions.id
	amount := renewInstructions.amount
	renterPubKey := renewInstructions.renterPubKey
	hostPubKey := renewInstructions.hostPubKey
	allowance := renter.Allowance

	// Get a session with the host, before marking it as being renewed.
	hs, err := c.Session(renterPubKey, hostPubKey, c.tg.StopChan())
	if err != nil {
		err = errors.AddContext(err, "Unable to establish session with host")
		return
	}
	s := hs.(*hostSession)

	// Mark the contract as being renewed, and defer logic to unmark it
	// once renewing is complete.
	c.log.Println("Marking a contract for renew:", id)
	c.mu.Lock()
	c.renewing[id] = true
	c.mu.Unlock()
	defer func() {
		c.log.Println("Unmarking the contract for renew", id)
		c.mu.Lock()
		delete(c.renewing, id)
		c.mu.Unlock()
	}()

	// Use the Settings RPC with the host and then invalidate the session.
	hostSettings, err := s.Settings()
	if err != nil {
		err = errors.AddContext(err, "Unable to get host settings")
		return
	}
	s.invalidate()

	// Perform the actual renewal. If the renewal succeeds, return the
	// contract. If the renewal fails we check how often it has failed
	// before. Once it has failed for a certain number of blocks in a
	// row and reached its second half of the renew window, we give up
	// on renewing it and set goodForRenew to false.
	c.log.Println("calling managedRenew on contract", id)
	newContract, errRenew := c.managedRenew(id, renterPubKey, hostPubKey, amount, endHeight, hostSettings)
	c.log.Println("managedRenew has returned with error:", errRenew)
	oldContract, exists := c.staticContracts.Acquire(id)
	if !exists {
		return types.ZeroCurrency, newContract, errors.AddContext(errContractNotFound, "failed to acquire oldContract after renewal")
	}
	oldUtility := oldContract.Utility()
	if errRenew != nil {
		// Increment the number of failed renewals for the contract if it
		// was the host's fault.
		if smodules.IsHostsFault(errRenew) {
			c.mu.Lock()
			c.numFailedRenews[oldContract.Metadata().ID]++
			totalFailures := c.numFailedRenews[oldContract.Metadata().ID]
			c.mu.Unlock()
			c.log.Println("remote host determined to be at fault, tallying up failed renews", totalFailures, id)
		}

		// Check if contract has to be replaced.
		md := oldContract.Metadata()
		c.mu.RLock()
		numRenews, failedBefore := c.numFailedRenews[md.ID]
		c.mu.RUnlock()
		secondHalfOfWindow := blockHeight + allowance.RenewWindow / 2 >= md.EndHeight
		replace := numRenews >= consecutiveRenewalsBeforeReplacement
		if failedBefore && secondHalfOfWindow && replace {
			oldUtility.GoodForRenew = false
			oldUtility.GoodForUpload = false
			oldUtility.Locked = true
			err := c.callUpdateUtility(oldContract, oldUtility, true)
			if err != nil {
				c.log.Println("WARN: failed to mark contract as !goodForRenew:", err)
			}
			c.log.Printf("WARN: consistently failed to renew %v, marked as bad and locked: %v\n",
				oldContract.Metadata().HostPublicKey, errRenew)
			c.staticContracts.Return(oldContract)
			return types.ZeroCurrency, newContract, errors.AddContext(errRenew, "contract marked as bad for too many consecutive failed renew attempts")
		}

		// Seems like it doesn't have to be replaced yet. Log the
		// failure and number of renews that have failed so far.
		c.log.Printf("WARN: failed to renew contract %v [%v]: '%v', current height: %v, proposed end height: %v, max duration: %v",
			oldContract.Metadata().HostPublicKey, numRenews, errRenew, blockHeight, endHeight, hostSettings.MaxDuration)
		c.staticContracts.Return(oldContract)
		return types.ZeroCurrency, newContract, errors.AddContext(errRenew, "contract renewal with host was unsuccessful")
	}
	c.log.Printf("Renewed contract %v\n", id)

	// Update the utility values for the new contract, and for the old
	// contract.
	newUtility := smodules.ContractUtility{
		GoodForUpload: true,
		GoodForRenew:  true,
	}
	if err := c.managedAcquireAndUpdateContractUtility(newContract.ID, newUtility); err != nil {
		c.log.Println("Failed to update the contract utilities", err)
		c.staticContracts.Return(oldContract)
		return amount, newContract, nil
	}
	oldUtility.GoodForRenew = false
	oldUtility.GoodForUpload = false
	oldUtility.Locked = true
	if err := c.callUpdateUtility(oldContract, oldUtility, true); err != nil {
		c.log.Println("Failed to update the contract utilities", err)
		c.staticContracts.Return(oldContract)
		return amount, newContract, nil
	}

	// Lock the contractor as we update it to use the new contract
	// instead of the old contract.
	c.mu.Lock()
	// Link Contracts.
	c.renewedFrom[newContract.ID] = id
	c.renewedTo[id] = newContract.ID
	// Store the contract in the record of historic contracts.
	c.oldContracts[id] = oldContract.Metadata()
	// Save the contractor.
	err = c.save()
	if err != nil {
		c.log.Println("Failed to save the contractor after creating a new contract.")
	}
	c.mu.Unlock()

	// Update the database.
	err = c.updateRenewedContract(id, newContract.ID)
	if err != nil {
		c.log.Println("Failed to update contracts in the database.")
	}

	// Delete the old contract.
	c.staticContracts.Delete(oldContract)

	// Signal to the watchdog that it should immediately post the last
	// revision for this contract.
	go c.staticWatchdog.threadedSendMostRecentRevision(oldContract.Metadata())
	return amount, newContract, nil
}

// managedAcquireAndUpdateContractUtility is a helper function that acquires a contract, updates
// its ContractUtility and returns the contract again.
func (c *Contractor) managedAcquireAndUpdateContractUtility(id types.FileContractID, utility smodules.ContractUtility) error {
	fileContract, ok := c.staticContracts.Acquire(id)
	if !ok {
		return errors.New("failed to acquire contract for update")
	}
	defer c.staticContracts.Return(fileContract)

	return c.managedUpdateContractUtility(fileContract, utility)
}

// managedUpdateContractUtility is a helper function that updates the contract
// with the given utility.
func (c *Contractor) managedUpdateContractUtility(fileContract *proto.FileContract, utility smodules.ContractUtility) error {
	// Sanity check to verify that we aren't attempting to set a good utility on
	// a contract that has been renewed.
	c.mu.Lock()
	_, exists := c.renewedTo[fileContract.Metadata().ID]
	c.mu.Unlock()
	if exists && (utility.GoodForRenew || utility.GoodForUpload) {
		c.log.Println("CRITICAL: attempting to update contract utility on a contract that has been renewed")
	}

	return c.callUpdateUtility(fileContract, utility, false)
}

// callUpdateUtility updates the utility of a contract. This method should
// *always* be used as opposed to calling UpdateUtility directly on a safe
// contract from the contractor. Pass in renewed as true if the contract
// has been renewed.
func (c *Contractor) callUpdateUtility(fileContract *proto.FileContract, newUtility smodules.ContractUtility, renewed bool) error {
	// TODO Think about implementing ChurnLimiter.

	return fileContract.UpdateUtility(newUtility)
}

// threadedContractMaintenance checks the set of contracts that the contractor
// has, dropping contracts which are no longer worthwhile.
//
// Between each network call, the thread checks whether a maintenance interrupt
// signal is being sent. If so, maintenance returns, yielding to whatever thread
// issued the interrupt.
func (c *Contractor) threadedContractMaintenance() {
	err := c.tg.Add()
	if err != nil {
		return
	}
	defer c.tg.Done()

	// No contract maintenance unless contractor is synced.
	if !c.managedSynced() {
		c.log.Println("Skipping contract maintenance since consensus isn't synced yet")
		return
	}
	c.log.Println("starting contract maintenance")

	// Only one instance of this thread should be running at a time. It is
	// fine to return early if another thread is already doing maintenance.
	// The next block will trigger another round.
	if !c.maintenanceLock.TryLock() {
		c.log.Println("maintenance lock could not be obtained")
		return
	}
	defer c.maintenanceLock.Unlock()

	// Perform general cleanup of the contracts. This includes archiving
	// contracts and other cleanup work.
	c.managedArchiveContracts()
	c.managedCheckForDuplicates()
	c.managedUpdatePubKeysToContractIDMap()
	c.managedPruneRedundantAddressRange()
	if err != nil {
		c.log.Println("Unable to mark contract utilities:", err)
		return
	}
	err = c.hdb.UpdateContracts(c.staticContracts.ViewAll())
	if err != nil {
		c.log.Println("Unable to update hostdb contracts:", err)
		return
	}
	c.managedLimitGFUHosts()
}

// FormContracts forms up to the specified number of contracts, puts them
// in the contract set, and returns them.
func (c *Contractor) FormContracts(rpk types.SiaPublicKey) ([]modules.RenterContract, error) {
	// No contract formation until the contractor is synced.
	if !c.managedSynced() {
		return nil, errors.New("contractor isn't synced yet")
	}

	// Check if we know this renter.
	c.mu.RLock()
	renter, exists := c.renters[rpk.String()]
	blockHeight := c.blockHeight
	c.mu.RUnlock()
	if !exists {
		return nil, ErrRenterNotFound
	}

	// Register or unregister and alerts related to contract formation.
	var registerLowFundsAlert bool
	defer func() {
		if registerLowFundsAlert {
			c.staticAlerter.RegisterAlert(smodules.AlertIDRenterAllowanceLowFunds, AlertMSGAllowanceLowFunds, AlertCauseInsufficientAllowanceFunds, smodules.SeverityWarning)
		} else {
			c.staticAlerter.UnregisterAlert(smodules.AlertIDRenterAllowanceLowFunds)
		}
	}()

	// Check if the renter has enough contracts according to their allowance.
	var fundsRemaining types.Currency
	numHosts := renter.Allowance.Hosts
	if numHosts == 0 {
		return nil, errors.New("zero number of hosts specified")
	}
	endHeight := blockHeight + renter.Allowance.Period + renter.Allowance.RenewWindow

	// Depend on the PeriodSpending function to get a breakdown of spending in
	// the contractor. Then use that to determine how many funds remain
	// available in the allowance.
	spending, err := c.PeriodSpending(renter.PublicKey)
	if err != nil {
		// This should only error if the contractor is shutting down.
		return nil, err
	}

	// Check for an underflow. This can happen if the user reduced their
	// allowance at some point to less than what we've already spent.
	fundsRemaining = renter.Allowance.Funds
	if spending.TotalAllocated.Cmp(fundsRemaining) < 0 {
		fundsRemaining = fundsRemaining.Sub(spending.TotalAllocated)
	}

	// Count the number of contracts which are good for uploading, and then make
	// more as needed to fill the gap.
	contractSet := make([]modules.RenterContract, 0, renter.Allowance.Hosts)
	uploadContracts := 0
	for _, contract := range c.staticContracts.ByRenter(renter.PublicKey) {
		if cu, ok := c.managedContractUtility(contract.ID); ok && cu.GoodForUpload {
			contractSet = append(contractSet, contract)
			uploadContracts++
			if uploadContracts >= int(renter.Allowance.Hosts) {
				break
			}
		}
	}
	neededContracts := int(renter.Allowance.Hosts) - uploadContracts
	if neededContracts <= 0 {
		return contractSet, nil
	}

	c.log.Println("need more contracts:", neededContracts)

	// Assemble two exclusion lists. The first one includes all hosts that we
	// already have contracts with and the second one includes all hosts we
	// have active contracts with. Then select a new batch of hosts to attempt
	// contract formation with.
	allContracts := c.staticContracts.ByRenter(renter.PublicKey)
	var blacklist []types.SiaPublicKey
	var addressBlacklist []types.SiaPublicKey
	for _, contract := range allContracts {
		blacklist = append(blacklist, contract.HostPublicKey)
		if !contract.Utility.Locked || contract.Utility.GoodForRenew || contract.Utility.GoodForUpload {
			addressBlacklist = append(addressBlacklist, contract.HostPublicKey)
		}
	}

	// Determine the max and min initial contract funding based on the
	// allowance settings.
	maxInitialContractFunds := renter.Allowance.Funds.Div64(renter.Allowance.Hosts).Mul64(MaxInitialContractFundingMulFactor).Div64(MaxInitialContractFundingDivFactor)
	minInitialContractFunds := renter.Allowance.Funds.Div64(renter.Allowance.Hosts).Div64(MinInitialContractFundingDivFactor)

	// Get Hosts.
	hosts, err := c.hdb.RandomHostsWithLimits(neededContracts * 4 + randomHostsBufferForScore, blacklist, addressBlacklist, renter.Allowance)
	if err != nil {
		return nil, err
	}

	// Calculate the anticipated transaction fee.
	_, maxFee := c.tpool.FeeEstimation()
	txnFee := maxFee.Mul64(smodules.EstimatedFileContractTransactionSetSize)

	// Form contracts with the hosts one at a time, until we have enough
	// contracts.
	for _, host := range hosts {
		// Return here if an interrupt or kill signal has been sent.
		select {
		case <-c.tg.StopChan():
			return nil, errors.New("the manager was stopped")
			default:
		}

		// If no more contracts are needed, break.
		if neededContracts <= 0 {
			break
		}

		// Calculate the contract funding with the host.
		contractFunds := host.ContractPrice.Add(txnFee).Mul64(ContractFeeFundingMulFactor)

		// Check that the contract funding is reasonable compared to the max and
		// min initial funding. This is to protect against increases to
		// allowances being used up to fast and not being able to spread the
		// funds across new contracts properly, as well as protecting against
		// contracts renewing too quickly.
		if contractFunds.Cmp(maxInitialContractFunds) > 0 {
			contractFunds = maxInitialContractFunds
		}
		if contractFunds.Cmp(minInitialContractFunds) < 0 {
			contractFunds = minInitialContractFunds
		}

		// Confirm that the wallet is unlocked.
		unlocked, err := c.wallet.Unlocked()
		if !unlocked || err != nil {
			return nil, errors.New("the wallet is locked")
		}

		// Determine if we have enough money to form a new contract.
		if fundsRemaining.Cmp(contractFunds) < 0 {
			registerLowFundsAlert = true
			c.log.Println("WARN: need to form new contracts, but unable to because of a low allowance")
			break
		}

		// Attempt forming a contract with this host.
		start := time.Now()
		fundsSpent, newContract, err := c.managedNewContract(renter.PublicKey, host, contractFunds, endHeight)
		if err != nil {
			c.log.Printf("Attempted to form a contract with %v, time spent %v, but negotiation failed: %v\n", host.NetAddress, time.Since(start).Round(time.Millisecond), err)
			continue
		}
		fundsRemaining = fundsRemaining.Sub(fundsSpent)
		neededContracts--

		// Lock the funds in the database.
		funds, _ := fundsSpent.Float64()
		hastings, _ := types.SiacoinPrecision.Float64()
		amount := funds / hastings
		err = c.satellite.LockSiacoins(renter.Email, amount)
		if err != nil {
			c.log.Println("ERROR: couldn't lock funds")
		}

		// Add this contract to the contractor and save.
		contractSet = append(contractSet, newContract)
		err = c.managedAcquireAndUpdateContractUtility(newContract.ID, smodules.ContractUtility{
			GoodForUpload: true,
			GoodForRenew:  true,
		})
		if err != nil {
			c.log.Println("Failed to update the contract utilities", err)
			continue
		}
		c.mu.Lock()
		err = c.save()
		c.mu.Unlock()
		if err != nil {
			c.log.Println("Unable to save the contractor:", err)
		}
	}

	return contractSet, nil
}

// RenewContracts tries to renew a given set of contracts.
func (c *Contractor) RenewContracts(rpk types.SiaPublicKey, contracts []types.FileContractID) ([]modules.RenterContract, error) {
	// No contract renewal until the contractor is synced.
	if !c.managedSynced() {
		return nil, errors.New("contractor isn't synced yet")
	}

	// Check if we know this renter.
	c.mu.RLock()
	renter, exists := c.renters[rpk.String()]
	blockHeight := c.blockHeight
	c.mu.RUnlock()
	if !exists {
		return nil, ErrRenterNotFound
	}

	// The total number of renews that failed for any reason.
	var numRenewFails int
	var renewErr error

	// Register or unregister and alerts related to contract renewal.
	var registerLowFundsAlert bool
	defer func() {
		if registerLowFundsAlert {
			c.staticAlerter.RegisterAlert(smodules.AlertIDRenterAllowanceLowFunds, AlertMSGAllowanceLowFunds, AlertCauseInsufficientAllowanceFunds, smodules.SeverityWarning)
		} else {
			c.staticAlerter.UnregisterAlert(smodules.AlertIDRenterAllowanceLowFunds)
		}
	}()

	var renewSet []fileContractRenewal
	var refreshSet []fileContractRenewal
	var fundsRemaining types.Currency

	// Iterate through the contracts. If the end height is not passed yet, and
	// if the contract is still GFU, add it to the resulting set.
	contractSet := make([]modules.RenterContract, 0, len(contracts))
	for _, id := range contracts {
		rc, ok := c.staticContracts.View(id)
		if !ok || rc.RenterPublicKey.String() != renter.PublicKey.String() {
			c.log.Println("WARN: contract ID submitted that doesn't belong to this renter:", id, renter.PublicKey.String())
			continue
		}

		cu, ok := c.managedContractUtility(id)
		if blockHeight + renter.Allowance.RenewWindow < rc.EndHeight && ok && cu.GoodForUpload {
			c.log.Println("INFO: contract is still GFU and hasn't expired yet:", id)
			contractSet = append(contractSet, rc)
			continue
		}

		// Create the renewSet and refreshSet. Each is a list of contracts that need
		// to be renewed, paired with the amount of money to use in each renewal.
		//
		// The renewSet is specifically contracts which are being renewed because
		// they are about to expire. And the refreshSet is contracts that are being
		// renewed because they are out of money.
		//
		// The contractor will prioritize contracts in the renewSet over contracts
		// in the refreshSet. If the wallet does not have enough money, or if the
		// allowance does not have enough money, the contractor will prefer to save
		// data in the long term rather than renew a contract.

		// Depend on the PeriodSpending function to get a breakdown of spending in
		// the contractor. Then use that to determine how many funds remain
		// available in the allowance for renewals.
		spending, err := c.PeriodSpending(renter.PublicKey)
		if err != nil {
			// This should only error if the contractor is shutting down.
			c.log.Println("WARN: error getting period spending:", err)
			return nil, err
		}

		// Check for an underflow. This can happen if the user reduced their
		// allowance at some point to less than what we've already spent.
		fundsRemaining = renter.Allowance.Funds
		if spending.TotalAllocated.Cmp(fundsRemaining) < 0 {
			fundsRemaining = fundsRemaining.Sub(spending.TotalAllocated)
		}

		// Skip any host that does not match our whitelist/blacklist filter
		// settings.
		host, _, err := c.hdb.Host(rc.HostPublicKey)
		if err != nil {
			c.log.Println("WARN: error getting host", err)
			continue
		}
		if host.Filtered {
			c.log.Println("Contract skipped because it is filtered")
			continue
		}
		// Skip hosts that can't use the current renter-host protocol.
		if build.VersionCmp(host.Version, smodules.MinimumSupportedRenterHostProtocolVersion) < 0 {
			c.log.Println("Contract skipped because host is using an outdated version", host.Version)
			continue
		}

		// Skip contracts which do not exist or are otherwise unworthy for
		// renewal.
		if !ok || !cu.GoodForRenew {
			c.log.Println("Contract skipped because it is not good for renew (utility.GoodForRenew, exists)", cu.GoodForRenew, ok)
			continue
		}

		// Calculate a spending for the contract that is proportional to how
		// much money was spend on the contract throughout this billing cycle
		// (which is now ending).
		if blockHeight + renter.Allowance.RenewWindow >= rc.EndHeight {
			renewAmount, err := c.managedEstimateRenewFundingRequirements(rc, blockHeight, renter.Allowance)
			if err != nil {
				c.log.Println("Contract skipped because there was an error estimating renew funding requirements", renewAmount, err)
				continue
			}
			renewSet = append(renewSet, fileContractRenewal{
				id:           rc.ID,
				amount:       renewAmount,
				renterPubKey: renter.PublicKey,
				hostPubKey:   rc.HostPublicKey,
			})
			c.log.Println("Contract has been added to the renew set for being past the renew height")
			continue
		}

		// Check if the contract is empty. We define a contract as being empty
		// if less than 'minContractFundRenewalThreshold' funds are remaining
		// (3% at time of writing), or if there is less than 3 sectors worth of
		// storage+upload+download remaining.
		blockBytes := types.NewCurrency64(smodules.SectorSize * uint64(renter.Allowance.Period))
		sectorStoragePrice := host.StoragePrice.Mul(blockBytes)
		sectorUploadBandwidthPrice := host.UploadBandwidthPrice.Mul64(smodules.SectorSize)
		sectorDownloadBandwidthPrice := host.DownloadBandwidthPrice.Mul64(smodules.SectorSize)
		sectorBandwidthPrice := sectorUploadBandwidthPrice.Add(sectorDownloadBandwidthPrice)
		sectorPrice := sectorStoragePrice.Add(sectorBandwidthPrice)
		percentRemaining, _ := big.NewRat(0, 1).SetFrac(rc.RenterFunds.Big(), rc.TotalCost.Big()).Float64()
		if rc.RenterFunds.Cmp(sectorPrice.Mul64(3)) < 0 || percentRemaining < MinContractFundRenewalThreshold {
			// Renew the contract with double the amount of funds that the
			// contract had previously. The reason that we double the funding
			// instead of doing anything more clever is that we don't know what
			// the usage pattern has been. The spending could have all occurred
			// in one burst recently, and the user might need a contract that
			// has substantially more money in it.
			//
			// We double so that heavily used contracts can grow in funding
			// quickly without consuming too many transaction fees, however this
			// does mean that a larger percentage of funds get locked away from
			// the user in the event that the user stops uploading immediately
			// after the renew.
			refreshAmount := rc.TotalCost.Mul64(2)
			minimum := renter.Allowance.Funds.MulFloat(fileContractMinimumFunding).Div64(renter.Allowance.Hosts)
			if refreshAmount.Cmp(minimum) < 0 {
				refreshAmount = minimum
			}
			refreshSet = append(refreshSet, fileContractRenewal{
				id:           rc.ID,
				amount:       refreshAmount,
				renterPubKey: renter.PublicKey,
				hostPubKey:   rc.HostPublicKey,
			})
			c.log.Println("Contract identified as needing to be refreshed:", rc.RenterFunds, sectorPrice.Mul64(3), percentRemaining, MinContractFundRenewalThreshold)
		}
	}
	if len(renewSet) != 0 || len(refreshSet) != 0 {
		c.log.Printf("renewing %v contracts and refreshing %v contracts\n", len(renewSet), len(refreshSet))
	}

	// Go through the contracts we've assembled for renewal. Any contracts that
	// need to be renewed because they are expiring (renewSet) get priority over
	// contracts that need to be renewed because they have exhausted their funds
	// (refreshSet). If there is not enough money available, the more expensive
	// contracts will be skipped.
	for _, renewal := range renewSet {
		// Return here if an interrupt or kill signal has been sent.
		select {
		case <-c.tg.StopChan():
			c.log.Println("returning because the manager was stopped")
			return nil, errors.New("the manager was stopped")
		default:
		}

		unlocked, err := c.wallet.Unlocked()
		if !unlocked || err != nil {
			c.log.Println("Contractor is attempting to renew contracts that are about to expire, however the wallet is locked")
			return nil, err
		}

		// Skip this renewal if we don't have enough funds remaining.
		if renewal.amount.Cmp(fundsRemaining) > 0 {
			c.log.Println("Skipping renewal because there are not enough funds remaining in the allowance", renewal.id, renewal.amount.HumanString(), fundsRemaining.HumanString())
			registerLowFundsAlert = true
			continue
		}

		// Renew one contract. The error is ignored because the renew function
		// already will have logged the error, and in the event of an error,
		// 'fundsSpent' will return '0'.
		fundsSpent, newContract, err := c.managedRenewContract(renewal, blockHeight, renter.ContractEndHeight())
		if errors.Contains(err, errContractNotGFR) {
			// Do not add a renewal error.
			c.log.Println("Contract skipped because it is not good for renew", renewal.id)
		} else if err != nil {
			c.log.Println("Error renewing a contract", renewal.id, err)
			renewErr = errors.Compose(renewErr, err)
			numRenewFails++
		}
		fundsRemaining = fundsRemaining.Sub(fundsSpent)

		if err == nil {
			// Lock the funds in the database.
			funds, _ := fundsSpent.Float64()
			hastings, _ := types.SiacoinPrecision.Float64()
			amount := funds / hastings
			err = c.satellite.LockSiacoins(renter.Email, amount)
			if err != nil {
				c.log.Println("ERROR: couldn't lock funds")
			}

			// Add this contract to the contractor and save.
			contractSet = append(contractSet, newContract)
			err = c.managedAcquireAndUpdateContractUtility(newContract.ID, smodules.ContractUtility{
				GoodForUpload: true,
				GoodForRenew:  true,
			})
			if err != nil {
				c.log.Println("Failed to update the contract utilities", err)
				continue
			}
			c.mu.Lock()
			err = c.save()
			c.mu.Unlock()
			if err != nil {
				c.log.Println("Unable to save the contractor:", err)
			}
		}
	}
	for _, renewal := range refreshSet {
		// Return here if an interrupt or kill signal has been sent.
		select {
		case <-c.tg.StopChan():
			c.log.Println("returning because the manager was stopped")
			return nil, errors.New("the manager was stopped")
		default:
		}
	
		unlocked, err := c.wallet.Unlocked()
		if !unlocked || err != nil {
			c.log.Println("contractor is attempting to refresh contracts that have run out of funds, however the wallet is locked")
			return nil, err
		}

		// Skip this renewal if we don't have enough funds remaining.
		if renewal.amount.Cmp(fundsRemaining) > 0 {
			c.log.Println("skipping refresh because there are not enough funds remaining in the allowance", renewal.id, renewal.amount.HumanString(), fundsRemaining.HumanString())
			registerLowFundsAlert = true
			continue
		}

		// Renew one contract. The error is ignored because the renew function
		// already will have logged the error, and in the event of an error,
		// 'fundsSpent' will return '0'.
		fundsSpent, newContract, err := c.managedRenewContract(renewal, blockHeight, renter.ContractEndHeight())
		if err != nil {
			c.log.Println("Error refreshing a contract", renewal.id, err)
			renewErr = errors.Compose(renewErr, err)
			numRenewFails++
		}
		fundsRemaining = fundsRemaining.Sub(fundsSpent)

		if err == nil {
			// Lock the funds in the database.
			funds, _ := fundsSpent.Float64()
			hastings, _ := types.SiacoinPrecision.Float64()
			amount := funds / hastings
			err = c.satellite.LockSiacoins(renter.Email, amount)
			if err != nil {
				c.log.Println("ERROR: couldn't lock funds")
			}

			// Add this contract to the contractor and save.
			contractSet = append(contractSet, newContract)
			err = c.managedAcquireAndUpdateContractUtility(newContract.ID, smodules.ContractUtility{
				GoodForUpload: true,
				GoodForRenew:  true,
			})
			if err != nil {
				c.log.Println("Failed to update the contract utilities", err)
				continue
			}
			c.mu.Lock()
			err = c.save()
			c.mu.Unlock()
			if err != nil {
				c.log.Println("Unable to save the contractor:", err)
			}
		}
	}

	// Update the failed renew map so that it only contains contracts which we
	// are currently trying to renew or refresh. The failed renew map is a map
	// that we use to track how many times consecutively we failed to renew a
	// contract with a host, so that we know if we need to abandon that host.
	c.mu.Lock()
	newFirstFailedRenew := make(map[types.FileContractID]types.BlockHeight)
	for _, r := range renewSet {
		if _, exists := c.numFailedRenews[r.id]; exists {
			newFirstFailedRenew[r.id] = c.numFailedRenews[r.id]
		}
	}
	for _, r := range refreshSet {
		if _, exists := c.numFailedRenews[r.id]; exists {
			newFirstFailedRenew[r.id] = c.numFailedRenews[r.id]
		}
	}
	c.numFailedRenews = newFirstFailedRenew
	c.mu.Unlock()

	return contractSet, nil
}
