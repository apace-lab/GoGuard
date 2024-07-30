package imports

import (
	"os"
	"testing"

	"github.com/bozhen-liu/gopa/internal/testenv"
)

func TestMain(m *testing.M) {
	testenv.ExitIfSmallMachine()
	os.Exit(m.Run())
}
