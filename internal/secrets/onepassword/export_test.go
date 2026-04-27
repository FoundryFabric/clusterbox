// export_test.go — exposes unexported types to the black-box test package.
// Compiled only when running tests; does not appear in the public API.
package onepassword

// NewWithSDKParts constructs a Provider with injected SDK handles for tests.
func NewWithSDKParts(cfg Config, s sdkSecretsAPI, i sdkItemsAPI, v sdkVaultsAPI) *Provider {
	return NewWithSDK(cfg, &sdkHandle{secrets: s, items: i, vaults: v})
}
