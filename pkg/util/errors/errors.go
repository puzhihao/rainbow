package errors

import (
	"errors"
	"gorm.io/gorm"
)

var (
	ErrRecordNotUpdate = errors.New("record not updated")
)

func IsNotUpdated(err error) bool {
	return errors.Is(err, ErrRecordNotUpdate)
}

func IsNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}
