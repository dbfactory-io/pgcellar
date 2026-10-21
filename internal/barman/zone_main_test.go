package barman

import (
	"testing"

	"github.com/dbfactory-io/pgcellar/internal/tztest"
)

// The tests run in the time zone of the Barman fixtures, whatever the zone of
// the machine (see internal/tztest).
func TestMain(m *testing.M) { tztest.Main(m) }
