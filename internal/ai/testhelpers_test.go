package ai

// NewCLIProvider returns a CLIProvider using the best available backend on PATH.
func NewCLIProvider() *CLIProvider {
	return NewCLIProviderWithBinaries("", "", "", "")
}
