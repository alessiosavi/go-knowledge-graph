// Package argerr marks invalid tool arguments while preserving error text.
package argerr

import (
	"errors"
	"fmt"
)

type Error struct{ error }

func (e Error) Unwrap() error { return e.error }

func Wrap(err error) error { return Error{err} }

func New(message string) error { return Wrap(errors.New(message)) }

func Errorf(format string, args ...any) error { return Wrap(fmt.Errorf(format, args...)) }
