package domain

import "fmt"

type Code string

const (
	CodeValidation             Code = "validation_error"
	CodeNotFound               Code = "not_found"
	CodeMenuItemNotFound       Code = "menu_item_not_found"
	CodeMenuItemUnavailable    Code = "menu_item_unavailable"
	CodeMenuItemPriceChanged   Code = "menu_item_price_changed"
	CodeMixedEstablishments    Code = "mixed_establishments"
	CodeIdempotencyKeyReused   Code = "idempotency_key_reused"
	CodeInvalidOrderTransition Code = "invalid_order_transition"
	CodeUnauthorized           Code = "unauthorized"
	CodeConflict               Code = "conflict"
)

type Error struct {
	Code    Code
	Message string
	Details map[string]any
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func NewError(code Code, message string, details map[string]any) *Error {
	return &Error{Code: code, Message: message, Details: details}
}
