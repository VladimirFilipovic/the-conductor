package cmd

import (
	"fmt"
	"os"
	"strings"
)

// Environment-variable fallbacks, consulted when the matching flag is empty.
const (
	envProject     = "CONDUCTOR_PROJECT"
	envEnvironment = "CONDUCTOR_ENVIRONMENT"
	envService     = "CONDUCTOR_SERVICE"
	envToken       = "CONDUCTOR_TOKEN" //nolint:unused // consumed by the engine client
)

// Context is the (project, environment, service) triple every command resolves
// before talking to the orchestration engine. There is no link file: identity
// comes only from flags or environment variables, with flags taking precedence.
type Context struct {
	Project     string
	Environment string
	Service     string
}

func (c Context) String() string {
	return fmt.Sprintf("%s / %s / %s", dash(c.Project), dash(c.Environment), dash(c.Service))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// require checks that the named fields are present, returning an error listing
// everything that is missing so the user learns the whole gap at once.
func (c Context) require(project, environment, service bool) error {
	var missing []string
	if project && c.Project == "" {
		missing = append(missing, "--project/-p (or "+envProject+")")
	}
	if environment && c.Environment == "" {
		missing = append(missing, "--environment/-e (or "+envEnvironment+")")
	}
	if service && c.Service == "" {
		missing = append(missing, "--service/-s (or "+envService+")")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required target: %s", strings.Join(missing, ", "))
	}
	return nil
}

// extractTarget pulls the universal -p/-e/-s (and long-form) flags out of args
// from any position, returning the remaining args plus the env-merged Context.
//
// Doing this manually — rather than via the flag package — lets the target
// flags appear anywhere on the line, e.g. `conductor variables set K=V -s web`,
// matching the Railway UX. Command-specific flags are left in the returned args
// for the command's own flag set to parse.
func extractTarget(args []string) ([]string, Context) {
	t := &Context{}
	var rest []string
	for i := 0; i < len(args); i++ {
		key, val, hasEq := splitFlag(args[i])
		switch key {
		case "-p", "--project":
			t.Project, i = takeVal(args, i, val, hasEq)
		case "-e", "--environment":
			t.Environment, i = takeVal(args, i, val, hasEq)
		case "-s", "--service":
			t.Service, i = takeVal(args, i, val, hasEq)
		default:
			rest = append(rest, args[i])
		}
	}
	return rest, Context{
		Project:     orEnv(t.Project, envProject),
		Environment: orEnv(t.Environment, envEnvironment),
		Service:     orEnv(t.Service, envService),
	}
}

// splitFlag decomposes a token into (key, value, hasEq). For "--service=web"
// it returns ("--service", "web", true); for "-s" it returns ("-s", "", false);
// for a bare positional it returns the token itself with hasEq=false.
func splitFlag(a string) (key, val string, hasEq bool) {
	if !strings.HasPrefix(a, "-") {
		return a, "", false
	}
	if k, v, ok := strings.Cut(a, "="); ok {
		return k, v, true
	}
	return a, "", false
}

// takeVal returns a flag's value and the new loop index — either the inline
// "=value" or the following token.
func takeVal(args []string, i int, val string, hasEq bool) (string, int) {
	if hasEq {
		return val, i
	}
	if i+1 < len(args) {
		return args[i+1], i + 1
	}
	return "", i
}

func orEnv(flagVal, key string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(key)
}
