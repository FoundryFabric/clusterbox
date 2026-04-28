package cmd

// staticTokenResolver is a TokenResolver for tests. It returns pre-set
// values and never calls the 1Password CLI. Missing keys return "".
type staticTokenResolver map[string]string

func (s staticTokenResolver) ResolveToken(key, _ string) (string, error) {
	return s[key], nil
}
