package errors

import (
	"github.com/bozhen-liu/gopa/internal/lsp/types"
)

func _() {
	bob.Bob() //@complete(".")
	types.b //@complete(" //", Bob_interface)
}
