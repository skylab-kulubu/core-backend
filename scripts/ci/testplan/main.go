// Command testplan splits core's Go tests over CI jobs (.github/workflows/
// checks.yml, docs/ci.md).
//
//	testplan affected [-all] < changed-paths       the packages whose tests a change can affect
//	testplan plan -packages F -timings F -out F    shards, as a GitHub Actions matrix on stdout
//	testplan run -plan F -shard N -dir D -- FLAGS  one shard's tests (go test FLAGS -json)
//
// A plan's unit is a package, or a part of a long package: some of its tests
// (go test -run), and in its last part every test the other parts do not run
// (go test -skip), so a test the plan did not foresee still runs.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: testplan affected|plan|run [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "affected":
		err = affectedCommand(os.Args[2:])
	case "plan":
		err = planCommand(os.Args[2:])
	case "run":
		err = runCommand(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "testplan:", err)
		os.Exit(1)
	}
}
