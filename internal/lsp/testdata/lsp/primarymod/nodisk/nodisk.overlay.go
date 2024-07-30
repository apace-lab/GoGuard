package nodisk

import (
	"github.com/bozhen-liu/gopa/internal/lsp/foo"
)

func _() {
	foo.Foo() //@complete("F", Foo, IntFoo, StructFoo)
}
