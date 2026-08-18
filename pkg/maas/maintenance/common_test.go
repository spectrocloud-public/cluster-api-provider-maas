package maintenance

import (
	"fmt"
	"os"
	"testing"

	maasclient "github.com/spectrocloud/maas-client-go/maasclient"
)

// Integration test for ListHostVMs. Skips unless MAAS_ENDPOINT, MAAS_API_KEY and
// TEST_MAAS_HOST_SYSTEM_ID are provided in the environment.
func TestListHostVMs_Integration(t *testing.T) {

	endpoint := os.Getenv("MAAS_ENDPOINT")
	apiKey := os.Getenv("MAAS_API_KEY")
	hostID := os.Getenv("TEST_MAAS_HOST_SYSTEM_ID")

	if endpoint == "" || apiKey == "" || hostID == "" {
		t.Skip("MAAS_ENDPOINT/MAAS_API_KEY/TEST_MAAS_HOST_SYSTEM_ID not set; skipping integration test")
	}

	client := maasclient.NewAuthenticatedClientSet(endpoint, apiKey)
	inv := NewInventoryService(client)

	vms, err := inv.ListHostVMs(hostID)
	if err != nil {
		t.Fatalf("ListHostVMs failed: %v", err)
	}

	// Validate that all returned VMs point to the requested host
	for _, vm := range vms {
		if vm.HostSystemID != hostID {
			t.Fatalf("VM %s reports HostSystemID=%s, expected %s", vm.SystemID, vm.HostSystemID, hostID)
		}

		fmt.Println("VM: ", vm.SystemID, " HostSystemID: ", vm.HostSystemID)
		fmt.Println("VM: ", vm)
	}

	t.Logf("ListHostVMs(%s) returned %d VMs", hostID, len(vms))
}
