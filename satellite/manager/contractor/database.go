package contractor

import (
	"github.com/mike76-dev/sia-satellite/modules"
)

// UpdateRenter updates the renter record in the database.
// The record must have already been created.
func (c *Contractor) UpdateRenter(renter modules.Renter) error {
	_, err := c.db.Exec(`
		UPDATE renters
		SET current_period = ?, funds = ?, hosts = ?, period = ?, renew_window = ?,
			expected_storage = ?, expected_upload = ?, expected_download = ?,
			expected_redundancy = ?, max_rpc_price = ?, max_contract_price = ?,
			max_download_bandwidth_price = ?, max_sector_access_price = ?,
			max_storage_price = ?, max_upload_bandwidth_price = ?
		WHERE email = ?
	`, uint64(renter.CurrentPeriod), renter.Allowance.Funds.String(), renter.Allowance.Hosts, uint64(renter.Allowance.Period), uint64(renter.Allowance.RenewWindow), renter.Allowance.ExpectedStorage, renter.Allowance.ExpectedUpload, renter.Allowance.ExpectedDownload, renter.Allowance.ExpectedRedundancy, renter.Allowance.MaxRPCPrice.String(), renter.Allowance.MaxContractPrice.String(), renter.Allowance.MaxDownloadBandwidthPrice.String(), renter.Allowance.MaxSectorAccessPrice.String(), renter.Allowance.MaxStoragePrice.String(), renter.Allowance.MaxUploadBandwidthPrice.String(), renter.Email)
	return err
}