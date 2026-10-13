package backenderr

import "fmt"

type ResourceError struct{ Status int }

func (e *ResourceError) Error() string {
	return fmt.Sprintf("backend temporarily unavailable (HTTP %d)", e.Status)
}

func FromStatus(status int) error {
	if status == 429 || status == 503 || status == 507 || status == 502 || status == 504 {
		return &ResourceError{Status: status}
	}
	return nil
}
