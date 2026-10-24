package filtered

import "os"

func Keep() { _ = os.Getenv("A") } // want: os.Getenv
