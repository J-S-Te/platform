package application

import "errors"

var (
	ErrNotFound      = errors.New("filetask resource not found")
	ErrConflict      = errors.New("filetask resource conflict")
	ErrValidation    = errors.New("filetask validation failed")
	ErrForbidden     = errors.New("filetask access forbidden")
	ErrStorage       = errors.New("filetask local storage failure")
	ErrPayloadUnsafe = errors.New("filetask job payload is unsafe")
)
