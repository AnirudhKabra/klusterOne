package cli

import (
	"fmt"
	"strings"
)

// splitPositional pulls the first positional argument off args (the NM name)
// and returns the rest for `flag.FlagSet.Parse`. This lets users put flags
// either before or after the name without hitting Go stdlib `flag`'s
// "stop at first non-flag" behavior.
//
// We assume exactly ONE positional. Anything else is either an error (if the
// first thing looks like a flag and we needed a name) or surfaced as a flag.
func splitPositional(args []string, subcmd, posUsage string) (string, []string, error) {
	if len(args) == 0 {
		return "", nil, fmt.Errorf("usage: kubectl nm %s %s [flags]", subcmd, posUsage)
	}
	if strings.HasPrefix(args[0], "-") {
		return "", nil, fmt.Errorf("usage: kubectl nm %s %s [flags] (positional argument must come first)", subcmd, posUsage)
	}
	return args[0], args[1:], nil
}

// hasHelpFlag reports whether args contains a help-request token in any
// position. Subcommands call this immediately after registering their flags
// to short-circuit normal parsing: print usage and exit 0 instead of failing
// because a help flag was placed before the positional name.
func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			return true
		}
	}
	return false
}
