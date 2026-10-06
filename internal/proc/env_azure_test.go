package proc

import (
	"strings"
	"testing"
)

// Azure DevOps tokens, and an Azure Pipelines job's System.AccessToken
// mapped into the environment, never reach the agents.
func TestWithoutSecretsAzure(t *testing.T) {
	in := []string{"AZURE_DEVOPS_TOKEN=pat", "AZURE_DEVOPS_EXT_PAT=ext", "SYSTEM_ACCESSTOKEN=job", "SYSTEM_COLLECTIONURI=https://dev.azure.com/org/", "TF_BUILD=True"}
	got := strings.Join(WithoutSecrets(in), " ")
	if got != "SYSTEM_COLLECTIONURI=https://dev.azure.com/org/ TF_BUILD=True" {
		t.Fatalf("got %s", got)
	}
}
