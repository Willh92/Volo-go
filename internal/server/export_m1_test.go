package server

import (
	"context"
	"net/netip"
)

func (s *AppServer) ValidateNormalizeAndPinSetupDBForTest(ctx context.Context, rawDriver, rawDSN string) (string, string, string, error) {
	return s.validateNormalizeAndPinSetupDB(ctx, rawDriver, rawDSN)
}

func PinSetupDSNToValidatedIPForTest(driver, dsn string, validIP netip.Addr, port int) string {
	return pinSetupDSNToValidatedIP(driver, dsn, validIP, port)
}
