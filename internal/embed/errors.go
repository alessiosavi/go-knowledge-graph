package embed

import (
	"errors"
	"fmt"
	"net/http"
)

var (
	ErrRateLimited = errors.New("embed: rate limited")
	ErrTransport   = errors.New("embed: transport failure")
)

// HTTPError is a non-success response from the single-request embed path.
type HTTPError struct {
	Status int
}

func (e HTTPError) Error() string { return fmt.Sprintf("embed: furls: HTTP %d", e.Status) }

func (e HTTPError) Is(target error) bool {
	return e.Status == http.StatusTooManyRequests && target == ErrRateLimited
}
