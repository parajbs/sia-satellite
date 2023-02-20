package provider

import (
	"time"
)

// formContractsTime defines the amount of time that the provider
// has to form contracts with the hosts.
const formContractsTime = 10 * time.Minute