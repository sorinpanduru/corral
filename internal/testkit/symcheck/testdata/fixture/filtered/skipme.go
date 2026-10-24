package filtered

import "os"

func Skip() { _ = os.Getenv("B") } // want-unfiltered: os.Getenv
