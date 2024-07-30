package good //@diag("package", "no_diagnostics", "", "error")

import (
	_ "go/ast"                                      //@prepare("go/ast", "_", "_")
	"github.com/bozhen-liu/gopa/internal/lsp/types" //@item(types_import, "types", "\"github.com/april1989/origin-go-tools/internal/lsp/types\"", "package")
)

func random() int { //@item(good_random, "random", "func() int", "func")
	_ = "random() int" //@prepare("random", "", "")
	y := 6 + 7         //@prepare("7", "", "")
	return y           //@prepare("return", "","")
}

func random2(y int) int { //@item(good_random2, "random2", "func(y int) int", "func"),item(good_y_param, "y", "int", "var")
	//@complete("", good_y_param, types_import, good_random, good_random2, good_stuff)
	var b types.Bob = &types.X{}   //@prepare("ypes","types", "types")
	if _, ok := b.(*types.X); ok { //@complete("X", X_struct, Y_struct, Bob_interface, CoolAlias)
	}

	return y
}
