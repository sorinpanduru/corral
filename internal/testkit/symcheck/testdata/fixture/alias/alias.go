package alias

import (
	. "os"
	osx "os"
	sc "syscall"
)

func Aliased() {
	_ = osx.Getenv("A") // want: os.Getenv
	_ = sc.Environ()    // want: syscall.Environ
}

func Dot() {
	_ = Getenv("A")  // want: os.Getenv
	_ = Args         // want: os.Args
	_, _ = Stat("f") // want: os.Stat
}

func MethodValue() {
	f := osx.Getenv // want: os.Getenv
	_ = f("A")
	use(osx.LookupEnv) // want: os.LookupEnv
}

func use(func(string) (string, bool)) {}
