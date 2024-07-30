package danglingstmt

import "github.com/bozhen-liu/gopa/internal/lsp/foo"

func _() {
	foo. //@rank(" //", Foo)
	var _ = []string{foo.} //@rank("}", Foo)
}
