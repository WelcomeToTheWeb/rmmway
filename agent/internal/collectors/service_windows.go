//go:build windows

package collectors

import (
	"context"

	"github.com/shirou/gopsutil/v4/winservices"
	"golang.org/x/sys/windows/svc"
)

// defaultServiceSampler (windows) queries the service control manager via
// gopsutil's winservices package — pure Go over the winapi, so the
// static-binary property holds. A service the SCM does not know is unknown
// (no sample); starting/running/continue-pending all count as "up" (a
// service that is coming up is not the one the playbook should restart).
func defaultServiceSampler(ctx context.Context, name string) (float64, error) {
	s, err := winservices.NewService(name)
	if err != nil {
		return 0, ErrServiceUnknown
	}
	st, err := s.QueryStatusWithContext(ctx)
	if err != nil {
		return 0, err
	}
	switch st.State {
	case svc.Running, svc.StartPending, svc.ContinuePending:
		return 1, nil
	default:
		return 0, nil
	}
}
