package settings

import (
	"flag"
	"strings"
)

// Flags registers one string flag per key that is not secret on fs, and
// returns a function that, after fs.Parse, gives the flags that were set,
// by key name, for Options.Flags.
func Flags(fs *flag.FlagSet) func() map[string]string {
	vals := map[string]*string{}
	for _, k := range Keys() {
		if k.IsSecret() {
			continue
		}
		vals[k.Name] = fs.String(k.Flag(), "", k.Desc+" Env "+k.Env()+".")
	}
	return func() map[string]string {
		out := map[string]string{}
		fs.Visit(func(f *flag.Flag) {
			for name, p := range vals {
				if flagName(name) == f.Name {
					out[name] = *p
				}
			}
		})
		return out
	}
}

// RefuseSecretFlags returns an error when args name a secret key as a flag,
// such as --llm-token. The flag would put the secret in argv, where ps and
// shell history show it.
func RefuseSecretFlags(args []string) error {
	for _, a := range args {
		if a == "--" {
			return nil
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		n := strings.TrimLeft(a, "-")
		n, _, _ = strings.Cut(n, "=")
		for _, k := range registry {
			if k.IsSecret() && n == flagName(k.Name) {
				return secretFlagError(k)
			}
		}
	}
	return nil
}
