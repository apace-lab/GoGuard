package c

import "github.com/bozhen-liu/gopa/internal/lsp/rename/b"

func _() {
	b.Hello() //@rename("Hello", "Goodbye")
}
