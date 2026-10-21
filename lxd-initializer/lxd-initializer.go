package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	lxdclient "github.com/canonical/lxd/client"
	"github.com/canonical/lxd/shared/api"
	"github.com/spectrocloud/maas-client-go/maasclient"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Common LXD socket paths
var lxdSocketPaths = []string{
	"/var/snap/lxd/common/lxd/unix.socket", // Snap path
}

// normalizeName converts a string to a DNS-safe-ish token: lowercase, non-alnum -> '-'
func normalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return s
	}
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	res := b.String()
	res = strings.Trim(res, "-")
	return res
}

// normalizeHost extracts host/IP from a MAAS power_address or raw string
func normalizeHost(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	h := u.Host
	if h == "" {
		h = u.Path
	}
	if hp, _, err2 := net.SplitHostPort(h); err2 == nil {
		h = hp
	}
	return h
}

// hostInterfaceExists returns true if an interface name exists on the host network namespace.
func hostInterfaceExists(ifName string) bool {
	if strings.TrimSpace(ifName) == "" {
		return false
	}
	cmd := exec.Command("nsenter", "-t", "1", "-m", "-p", "--", "ip", "link", "show", ifName)
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

// getKubernetesClient returns a Kubernetes client using in-cluster config
func getKubernetesClient() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %v", err)
	}

	return clientset, nil
}

// getMaasCredentialsFromSecret reads MAAS credentials from the capmaas-manager-bootstrap-credentials secret
// It searches across all namespaces to find the secret
func getMaasCredentialsFromSecret() (string, string, error) {
	client, err := getKubernetesClient()
	if err != nil {
		return "", "", fmt.Errorf("failed to get kubernetes client: %v", err)
	}

	secretName := "capmaas-manager-bootstrap-credentials"

	// List all secrets across all namespaces
	secretList, err := client.CoreV1().Secrets("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return "", "", fmt.Errorf("failed to list secrets: %v", err)
	}

	// Search for the capmaas-manager-bootstrap-credentials secret
	for _, secret := range secretList.Items {
		if secret.Name == secretName {
			maasEndpoint := string(secret.Data["MAAS_ENDPOINT"])
			maasAPIKey := string(secret.Data["MAAS_API_KEY"])

			if maasEndpoint == "" || maasAPIKey == "" {
				log.Printf("Warning: Found secret %s in namespace %s but MAAS_ENDPOINT or MAAS_API_KEY is empty", secretName, secret.Namespace)
				continue
			}

			log.Printf("Found MAAS credentials in secret %s in namespace %s", secretName, secret.Namespace)
			return maasEndpoint, maasAPIKey, nil
		}
	}

	return "", "", fmt.Errorf("secret %s not found in any namespace", secretName)
}

// extractSystemIDFromNodeName extracts system ID from MAAS node name
func extractSystemIDFromNodeName(nodeName string) (string, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return "", fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	ctx := context.Background()
	node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	providerID := node.Spec.ProviderID
	if err != nil {
		return "", fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	if !strings.HasPrefix(providerID, "maas:///") {
		return "", fmt.Errorf("invalid MAAS providerID format: %s", providerID)
	}

	parts := strings.Split(providerID, "/")
	if len(parts) < 4 {
		return "", fmt.Errorf("invalid MAAS providerID format")
	}

	systemID := parts[len(parts)-1]
	if systemID == "" {
		return "", fmt.Errorf("empty system ID in providerID: %s", providerID)
	}

	return systemID, nil
}

// getStorageSizeFromMaas retrieves storage size from MAAS for the current node
func getStorageSizeFromMaas(nodeName, maasAPIKey, maasEndpoint string) (string, error) {
	if nodeName == "" || maasAPIKey == "" || maasEndpoint == "" {
		return "", fmt.Errorf("missing required parameters")
	}
	ctx := context.Background()
	systemID, err := extractSystemIDFromNodeName(nodeName)
	if err != nil {
		return "", fmt.Errorf("failed to extract system ID: %w", err)
	}

	client := maasclient.NewAuthenticatedClientSet(maasEndpoint, maasAPIKey)
	machineClient := client.Machines().Machine(systemID)
	machine, err := machineClient.Get(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get machine details from MAAS: %w", err)
	}

	// Get total storage size
	totalStorageGB := machine.TotalStorageGB()
	if totalStorageGB == 0 {
		return "", fmt.Errorf("no storage found")
	}

	// Use 80% of total storage for LXD pool (leave space for OS)
	lxdStorageGB := totalStorageGB * 0.8
	log.Printf("Using storage size: %.0fGB (%.0f%% of total)", lxdStorageGB, lxdStorageGB/totalStorageGB*100)

	return strconv.FormatFloat(lxdStorageGB, 'f', 0, 64), nil
}

// ResourcePool represents a MAAS resource pool
type ResourcePool struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// getMachineInfoFromMaas gets zone, resource pool, and boot interface from MAAS for the current node
func getMachineInfoFromMaas(nodeName, maasAPIKey, maasEndpoint string) (zone, resourcePool, bootInterface string, err error) {
	if nodeName == "" || maasAPIKey == "" || maasEndpoint == "" {
		return "", "", "", fmt.Errorf("missing required parameters")
	}
	ctx := context.Background()
	systemID, err := extractSystemIDFromNodeName(nodeName)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to extract system ID: %w", err)
	}

	client := maasclient.NewAuthenticatedClientSet(maasEndpoint, maasAPIKey)
	machine, err := client.Machines().Machine(systemID).Get(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get machine details from MAAS: %w", err)
	}

	// Get zone
	if machine.Zone() != nil {
		zone = machine.Zone().Name()
	}

	resourcePool = machine.ResourcePoolName()
	bootInterface = machine.BootInterfaceName()

	return zone, resourcePool, bootInterface, nil
}

// registerWithMAAS registers the node as an LXD VM host in MAAS (idempotent)
func registerWithMAAS(maasEndpoint, maasAPIKey, systemID, nodeIP, trustPassword, project, zone, resourcePool, hostName string) error {
	if maasEndpoint == "" || maasAPIKey == "" {
		return fmt.Errorf("MAAS credentials unavailable")
	}
	ctx := context.Background()
	client := maasclient.NewAuthenticatedClientSet(maasEndpoint, maasAPIKey)

	// Idempotency/conflict checks via API for speed
	hosts, err := client.VMHosts().List(ctx, nil)
	if err != nil {
		return fmt.Errorf("list vm hosts: %w", err)
	}
	// Align with manual flow: bare IP for power_address (still used for create below)
	wantHost := nodeIP
	// Strict guards: rely only on name and system-id for idempotency
	expectedName := hostName
	for _, h := range hosts {
		// 1) Exact name match → idempotent or conflict
		if h.Name() == expectedName {
			if h.HostSystemID() == "" || h.HostSystemID() == systemID {
				log.Printf("MAAS VM host already present (name=%s, system-id=%s); skipping re-registration", h.Name(), h.HostSystemID())
				return nil
			}
			return fmt.Errorf("conflict: VM host %q belongs to system-id %s (expected %s)", h.Name(), h.HostSystemID(), systemID)
		}
		// 2) Same system already registered under a different name → idempotent
		if h.HostSystemID() == systemID {
			log.Printf("MAAS VM host for system-id=%s already exists under name=%s; skipping re-registration", systemID, h.Name())
			return nil
		}
		// 3) Non-matching entry → ignore
	}

	// Prefer MAAS CLI for creation to match manual success path
	if _, err := exec.LookPath("maas"); err == nil {
		profile := "ds"
		// Non-interactive login (idempotent)
		_ = runCmd("maas", []string{"login", profile, maasEndpoint, maasAPIKey})
		args := []string{profile, "vm-hosts", "create", "type=lxd", fmt.Sprintf("power_address=%s", wantHost)}
		if trustPassword != "" {
			args = append(args, fmt.Sprintf("password=%s", trustPassword))
		}
		args = append(args, fmt.Sprintf("name=%s", hostName))
		if project != "" {
			args = append(args, fmt.Sprintf("project=%s", project))
		}
		if zone != "" {
			args = append(args, fmt.Sprintf("zone=%s", zone))
		}
		if resourcePool != "" {
			args = append(args, fmt.Sprintf("pool=%s", resourcePool))
		}
		if err := runCmd("maas", args); err != nil {
			return fmt.Errorf("maas cli create failed: %w", err)
		}
		log.Printf("MAAS VM host registered via CLI: %s (%s)", hostName, wantHost)
		return nil
	}

	// Fallback: API create with project/zone/pool
	params := maasclient.ParamsBuilder().
		Set("type", "lxd").
		Set("power_address", wantHost).
		Set("name", hostName)
	if trustPassword != "" {
		params.Set("password", trustPassword)
	}
	if project != "" {
		params.Set("project", project)
	}
	if zone != "" {
		params.Set("zone", zone)
	}
	if resourcePool != "" {
		params.Set("pool", resourcePool)
	}
	created, err := client.VMHosts().Create(ctx, params)
	if err != nil {
		return fmt.Errorf("create vm host: %w", err)
	}

	// Post-create verify/correct scoping in case MAAS defaulted values
	if got, gerr := created.Get(ctx); gerr == nil {
		// Ensure ownership did not drift to a different system-id
		if got.HostSystemID() != "" && got.HostSystemID() != systemID {
			return fmt.Errorf("conflict after create: VM host mapped to system-id %s (expected %s)", got.HostSystemID(), systemID)
		}
		needUpdate := false
		upd := maasclient.ParamsBuilder()

		if project != "" {
			has := false
			for _, p := range got.Projects() {
				if p == project {
					has = true
					break
				}
			}
			if !has {
				upd.Set("project", project)
				needUpdate = true
			}
		}
		if zone != "" {
			z := ""
			if got.Zone() != nil {
				z = got.Zone().Name()
			}
			if z != zone {
				upd.Set("zone", zone)
				needUpdate = true
			}
		}
		if resourcePool != "" {
			pl := ""
			if got.ResourcePool() != nil {
				pl = got.ResourcePool().Name()
			}
			if pl != resourcePool {
				upd.Set("pool", resourcePool)
				needUpdate = true
			}
		}
		if needUpdate {
			if _, uerr := created.Update(ctx, upd); uerr != nil {
				log.Printf("Warning: VM host update to set project/zone/pool failed: %v", uerr)
			}
		}
	}

	log.Printf("MAAS VM host registered via API: %s (%s) project=%s zone=%s pool=%s", hostName, wantHost, project, zone, resourcePool)
	return nil
}

func runCmd(bin string, args []string) error {
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %s", bin, args, string(out))
	}
	return nil
}

func main() {
	log.Println("Starting LXD initializer")

	// Define command-line flags
	action := flag.String("action", "", "Action to perform: init, register, or both")
	storageBackendFlag := flag.String("storage-backend", "", "Storage backend (dir, zfs)")
	networkBridgeFlag := flag.String("network-bridge", "", "Network bridge name")
	skipNetworkUpdateFlag := flag.Bool("skip-network-update", false, "Skip updating existing network")
	nodeIPFlag := flag.String("node-ip", "", "Node IP address for registration")
	maasEndpointFlag := flag.String("maas-endpoint", "", "MAAS API endpoint")
	maasAPIKeyFlag := flag.String("maas-api-key", "", "MAAS API key")
	trustPasswordFlag := flag.String("trust-password", "", "Trust password for LXD")

	flag.Parse()

	// Get environment variables if flags are not set
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		log.Println("NODE_NAME environment variable is not set")
	}

	nodeIP := *nodeIPFlag
	if nodeIP == "" {
		nodeIP = os.Getenv("NODE_IP")
		if nodeIP == "" && nodeName != "" {
			nodeIP = nodeName // Fallback to node name if IP is not provided
		}
	}

	storageBackend := *storageBackendFlag
	if storageBackend == "" {
		storageBackend = os.Getenv("STORAGE_BACKEND")
		if storageBackend == "" {
			storageBackend = "zfs" // Default to ZFS
		}
	}

	var storageSize string

	// Auto-detect Storage Size of a bare metal machine
	maasAPIKey := *maasAPIKeyFlag
	maasEndpoint := *maasEndpointFlag

	// If flags are not provided, use env (set by DS rendering) or try to read from the Kubernetes secret
	if maasEndpoint == "" {
		maasEndpoint = os.Getenv("MAAS_ENDPOINT")
	}
	if maasAPIKey == "" {
		maasAPIKey = os.Getenv("MAAS_API_KEY")
	}
	if maasAPIKey == "" || maasEndpoint == "" {
		if secretEndpoint, secretAPIKey, err := getMaasCredentialsFromSecret(); err == nil {
			if maasEndpoint == "" {
				maasEndpoint = secretEndpoint
			}
			if maasAPIKey == "" {
				maasAPIKey = secretAPIKey
			}
		} else {
			log.Printf("Warning: Failed to get MAAS credentials from secret: %v", err)
		}
	}

	if maasAPIKey != "" && maasEndpoint != "" {
		if maasStorageSize, err := getStorageSizeFromMaas(nodeName, maasAPIKey, maasEndpoint); err == nil {
			storageSize = maasStorageSize
			log.Printf("Using storage size from MAAS: %s GB", storageSize)
		} else {
			log.Printf("Warning: Failed to get storage size from MAAS: %v, using default", err)
			storageSize = "50"
		}
	} else {
		log.Printf("Warning: MAAS API credentials not available, using default storage size")
		storageSize = "50"
	}

	// Determine NIC type and parent
	// NIC_TYPE env supports values like "bridge"/"bridged" or "macvlan". Default to bridge.
	nicTypeEnv := os.Getenv("NIC_TYPE")
	if nicTypeEnv == "" {
		nicTypeEnv = "bridge"
	}
	nicMode := strings.ToLower(nicTypeEnv)
	if nicMode == "bridged" {
		nicMode = "bridge"
	}

	networkBridge := *networkBridgeFlag
	if networkBridge == "" {
		networkBridge = os.Getenv("NETWORK_BRIDGE")
		if networkBridge == "" {
			networkBridge = "br0" // Default to br0
		}
	}

	// Auto-detect zone, resource pool, and boot interface name from MAAS
	zone, resourcePool, bootInterfaceName, err := getMachineInfoFromMaas(nodeName, maasAPIKey, maasEndpoint)
	if err != nil {
		log.Fatalf("Failed to get machine information from MAAS: %v", err)
	}
	log.Printf("Zone retrieved from MAAS: %s", zone)
	log.Printf("Resource pool retrieved from MAAS: %s", resourcePool)
	log.Printf("Boot interface retrieved from MAAS: %s", bootInterfaceName)

	// Project: default to "maas" (can be env-overridden later if needed)
	project := "maas"
	if p := strings.TrimSpace(os.Getenv("LXD_PROJECT")); p != "" {
		project = p
	}

	nicParent := os.Getenv("NIC_PARENT")
	if nicParent == "" {
		nicParent = bootInterfaceName
	}

	// Log final NIC config
	log.Printf("Using NIC mode=%s (device nictype=%s) parent=%s project=%s", nicMode, func() string {
		if nicMode == "bridge" {
			return "bridged"
		} else {
			return nicMode
		}
	}(), nicParent, project)

	skipNetworkUpdate := *skipNetworkUpdateFlag
	if !skipNetworkUpdate {
		skipNetworkUpdateEnv := os.Getenv("SKIP_NETWORK_UPDATE")
		if skipNetworkUpdateEnv == "true" || skipNetworkUpdateEnv == "1" || skipNetworkUpdateEnv == "yes" {
			skipNetworkUpdate = true
		}
	}

	trustPassword := *trustPasswordFlag
	if trustPassword == "" {
		trustPassword = os.Getenv("TRUST_PASSWORD")
	}
	// Always derive a per-node unique trust password from provided seed + systemID
	if sysID, err := extractSystemIDFromNodeName(nodeName); err == nil && strings.TrimSpace(trustPassword) != "" {
		trustPassword = trustPassword + ":" + sysID + ":" + nodeName
		log.Println("Derived per-node trust password from provided seed")
	}

	// Determine action based on flag or default to both
	actionStr := *action
	if actionStr == "" {
		actionStr = "both" // Default to both init and register
	}

	// Early exit: if node already marked initialized, skip all work. The label can go
	// stale (host reimaged, LXD snap removed), so it is only trusted when LXD is
	// actually functional - otherwise the node would never re-register with MAAS.
	if nodeName != "" {
		if client, err := getKubernetesClient(); err == nil {
			if node, gerr := client.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{}); gerr == nil {
				if node.Labels != nil {
					if node.Labels[LXDHostInitializedLabel] == LabelValueTrue {
						if verr := validateLXDFunctional(); verr != nil {
							log.Printf("Node %s labeled %s=true but LXD is not functional (%v); re-running initializer", nodeName, LXDHostInitializedLabel, verr)
						} else {
							log.Printf("Node %s already labeled %s=true; skipping initializer", nodeName, LXDHostInitializedLabel)
							log.Printf("Sleeping for 1 hour to keep the container running")
							time.Sleep(3600 * time.Second)
							return
						}
					}
				}
			}
		}
	}

	// Perform actions based on the specified action
	if actionStr == "init" || actionStr == "both" {
		// Initialize LXD
		if err := initializeLXD(storageBackend, storageSize, networkBridge, skipNetworkUpdate, trustPassword, nicMode, nicParent, project, nodeIP); err != nil {
			log.Fatalf("Failed to initialize LXD: %v", err)
		}

		// Do not mark initialized here; labeling will occur only after successful registration
	}

	// Add a fixed delay after init before registration when doing both steps
	if actionStr == "both" {
		delay := 30 * time.Second
		//log.Printf("LXD init complete; staggering for %v before host registration (index=%d/%d, per=%ds, cap=%ds, jitter<=%ds)", delay, nodeIndex, nodeCount, perNodeSec, maxCapSec, jitterSec)
		log.Printf("LXD init complete; staggering for %v before host registration", delay)
		time.Sleep(delay)
	}

	if actionStr == "register" || actionStr == "both" {
		// Single naming convention: lxd-host-<hostname>
		systemID, sErr := extractSystemIDFromNodeName(nodeName)
		if sErr != nil {
			log.Fatalf("Failed to extract system ID from node name: %v", sErr)
		}
		hostToken := normalizeName(nodeName)
		if hostToken == "" {
			hostToken = "node"
		}
		hostName := fmt.Sprintf("lxd-host-%s", hostToken)
		if err := registerWithMAAS(maasEndpoint, maasAPIKey, systemID, nodeIP, trustPassword, project, zone, resourcePool, hostName); err != nil {
			log.Fatalf("Failed to register LXD host in MAAS: %v", err)
		}

		// Validate LXD is functional before labeling
		log.Println("Validating LXD is functional before labeling node...")
		if err := validateLXDFunctional(); err != nil {
			log.Printf("ERROR: LXD registration succeeded but LXD is not functional: %v", err)
			log.Printf("NOT labeling node as initialized - LXD needs to be properly initialized")
			log.Fatalf("LXD validation failed after registration: %v", err)
		}

		// Label the node only after successful registration AND validation
		if nodeName != "" {
			nodeLabeler, err := NewNodeLabeler(nodeName)
			if err != nil {
				log.Printf("Warning: Failed to create node labeler for %s: %v", nodeName, err)
			} else {
				nodeLabeler.SafeMarkLXDInitialized()
				log.Printf("Node %s successfully labeled as LXD initialized after validation", nodeName)
			}
		}
	}

	// If running as a standalone binary, exit after completing the actions
	if actionStr == "once" {
		log.Println("Actions completed successfully")
		return
	}

	log.Println("Entering sleep loop to keep the container running")
	for {
		time.Sleep(3600 * time.Second)
	}
}

// findLXDSocket finds the LXD socket path
func findLXDSocket() (string, error) {
	for _, socketPath := range lxdSocketPaths {
		if _, err := os.Stat(socketPath); err == nil {
			return socketPath, nil
		}
	}
	return "", fmt.Errorf("no LXD socket found at any of the expected paths")
}

// waitForLXDSocket keeps checking well-known paths until a socket is found or the timeout expires.
func waitForLXDSocket(timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p, err := findLXDSocket(); err == nil {
			return p, nil
		}
		time.Sleep(time.Second)
	}
	return "", fmt.Errorf("timed out waiting (%s) for LXD socket", timeout)
}

// logLXDDiagnostics prints useful information when the LXD socket cannot be found.
func logLXDDiagnostics() {
	log.Println("===== LXD diagnostics =====")
	for _, p := range lxdSocketPaths {
		if fi, err := os.Stat(p); err == nil {
			log.Printf("candidate %s exists (size %d)", p, fi.Size())
		} else if os.IsNotExist(err) {
			log.Printf("candidate %s missing", p)
		} else {
			log.Printf("candidate %s error: %v", p, err)
		}
	}

	// host daemon status
	out, err := exec.Command("nsenter", "-t", "1", "-m", "-p", "--", "snap", "services", "lxd").CombinedOutput()
	if err == nil {
		log.Printf("snap services lxd:\n%s", string(out))
	} else {
		log.Printf("nsenter snap services failed: %v", err)
	}

	// process list
	if psOut, err := exec.Command("ps", "-ef").CombinedOutput(); err == nil {
		log.Printf("process list:\n%s", string(psOut))
	}
	log.Println("===== end diagnostics =====")
}

// setTrustPassword sets core.trust_password and verifies it.
func setTrustPassword(pw string) error {
	if pw == "" {
		return nil
	}
	// try inside container first
	cmd := exec.Command("/snap/bin/lxc", "config", "set", "core.trust_password", pw)
	if _, err := cmd.CombinedOutput(); err != nil {
		log.Printf("lxc trust_password failed inside container (%v), trying nsenter", err)
		cmd = exec.Command("nsenter", "-t", "1", "-m", "-p", "--", "/snap/bin/lxc", "config", "set", "core.trust_password", pw)
		if out2, err2 := cmd.CombinedOutput(); err2 != nil {
			return fmt.Errorf("failed to set trust password: %s: %w", string(out2), err2)
		}
	}
	// verify
	verifyCmd := exec.Command("nsenter", "-t", "1", "-m", "-p", "--", "/snap/bin/lxc", "config", "get", "core.trust_password")
	verifyOut, err := verifyCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("verification get failed: %s: %w", string(verifyOut), err)
	}
	if strings.TrimSpace(string(verifyOut)) != "true" {
		return fmt.Errorf("verification failed, expected 'true', got %q", strings.TrimSpace(string(verifyOut)))
	}
	log.Println("trust password verified present")
	return nil
}

// initializeLXD initializes LXD with the specified configuration
func initializeLXD(storageBackend, storageSize, networkBridge string, skipNetworkUpdate bool, trustPassword, nicType, nicParent, project, hostIP string) error {
	log.Printf("Initializing LXD with storage backend %s, size %sGB, and network bridge %s",
		storageBackend, storageSize, networkBridge)

	if skipNetworkUpdate {
		log.Println("Network updates will be skipped for existing networks")
	}

	// Wait for LXD socket to appear (up to 2 minutes)
	socketPath, err := waitForLXDSocket(2 * time.Minute)
	if err != nil {
		logLXDDiagnostics()
		return err
	}

	log.Printf("Found LXD socket at %s", socketPath)

	// Connect to LXD
	c, err := lxdclient.ConnectLXDUnix(socketPath, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to LXD: %w", err)
	}

	// Ensure project exists and get project-scoped server
	cProj := c
	if project != "" && project != "default" {
		if err := ensureLXDProject(c, project); err != nil {
			log.Printf("Warning: failed to ensure project %s: %v", project, err)
		}
		cProj = c.UseProject(project)
	}

	// Check if LXD is already initialized
	server, _, err := c.GetServer()
	if err != nil {
		return fmt.Errorf("failed to get server info: %w", err)
	}

	// Check LXD version to determine the approach
	apiVersion := server.APIVersion
	log.Printf("LXD API Version: %s", apiVersion)

	// Check for storage pools and networks (global/default)
	storagePoolsResponse, err := c.GetStoragePools()
	if err != nil {
		log.Printf("Warning: Failed to get storage pools: %v", err)
	}

	networksResponse, err := c.GetNetworks()
	if err != nil {
		log.Printf("Warning: Failed to get networks: %v", err)
	}

	// Check if storage pool exists
	storagePoolExists := false
	storagePoolName := "default"

	for _, pool := range storagePoolsResponse {
		if pool.Name == storagePoolName {
			storagePoolExists = true
			log.Printf("Storage pool '%s' already exists with driver '%s'", pool.Name, pool.Driver)
			break
		}
	}

	if !storagePoolExists {
		log.Printf("Creating storage pool '%s' with driver '%s'", storagePoolName, storageBackend)

		// Create storage pool
		storagePoolReq := api.StoragePoolsPost{
			Name:   storagePoolName,
			Driver: storageBackend,
			StoragePoolPut: api.StoragePoolPut{
				Config: map[string]string{
					"size": storageSize + "GB",
				},
			},
		}
		if err = c.CreateStoragePool(storagePoolReq); err != nil {
			return fmt.Errorf("failed to create storage pool: %w", err)
		}
		log.Printf("Storage pool '%s' created successfully", storagePoolName)
	} else {
		// pool exists, ensure size matches requested
		pool, etag, err := c.GetStoragePool(storagePoolName)
		if err == nil {
			desired := storageSize + "GB"
			if pool.Config["size"] != desired {
				log.Printf("Updating storage pool '%s' size from %s to %s", storagePoolName, pool.Config["size"], desired)
				poolPut := pool.Writable()
				if poolPut.Config == nil {
					poolPut.Config = map[string]string{}
				}
				poolPut.Config["size"] = desired
				if err := c.UpdateStoragePool(storagePoolName, poolPut, etag); err != nil {
					log.Printf("Warning: failed to update storage pool size: %v", err)
				} else {
					log.Printf("Storage pool '%s' size updated", storagePoolName)
				}
			}
		}
	}

	// Network configuration based on nicType (global/default project)
	// Use NETWORK_BRIDGE (if provided) for LXD-managed bridges; otherwise, attach VMs directly to the physical NIC (nicParent)
	effectiveParent := nicParent
	if nicType == "bridge" {
		bridgeName := strings.TrimSpace(networkBridge)
		if bridgeName != "" {
			// Auto-detect collision: if bridgeName equals the physical parent or an interface by that name
			// already exists on the host, skip creating/updating the LXD bridge and attach directly to nicParent.
			if bridgeName == nicParent || hostInterfaceExists(bridgeName) {
				log.Printf("Skipping LXD network create/update for '%s' due to collision with host interface/parent; using parent '%s'", bridgeName, nicParent)
			} else {
				effectiveParent = bridgeName
				if skipNetworkUpdate {
					log.Printf("skip-network-update=true; not creating/updating LXD network '%s'", bridgeName)
				} else {
					// Check if network bridge exists
					networkExists := false
					var existingNetworkType string

					for _, network := range networksResponse {
						if network.Name == bridgeName {
							networkExists = true
							existingNetworkType = network.Type
							log.Printf("Network '%s' already exists with type '%s'", network.Name, network.Type)
							break
						}
					}

					if !networkExists {
						log.Printf("Creating network '%s'", bridgeName)

						// Create network
						networkReq := api.NetworksPost{
							Name: bridgeName,
							Type: "bridge",
							NetworkPut: api.NetworkPut{
								Config: map[string]string{
									"ipv4.address": "auto",
									"ipv4.nat":     "true",
								},
							},
						}

						if err := c.CreateNetwork(networkReq); err != nil {
							return fmt.Errorf("failed to create network: %w", err)
						}
						log.Printf("Network '%s' created successfully", bridgeName)
					} else {
						// Only try to update if it's a managed bridge
						if existingNetworkType == "bridge" {
							// Update existing network
							network, etag, err := c.GetNetwork(bridgeName)
							if err != nil {
								log.Printf("Warning: Failed to get network details for update: %v", err)
								log.Println("Skipping network update")
							} else {
								// Ensure ipv4.nat is set to "true"
								networkPut := api.NetworkPut{Config: network.Config}
								if networkPut.Config == nil {
									networkPut.Config = make(map[string]string)
								}
								networkPut.Config["ipv4.nat"] = "true"
								if err = c.UpdateNetwork(bridgeName, networkPut, etag); err != nil {
									log.Printf("Warning: Failed to update network: %v", err)
									log.Println("Continuing without network update")
								} else {
									log.Printf("Network '%s' updated successfully", bridgeName)
								}
							}
						} else {
							log.Printf("Network '%s' is type '%s', not updating (only bridge networks can be updated)", bridgeName, existingNetworkType)
						}
					}
				}
			}
		} else {
			log.Printf("NETWORK_BRIDGE not set; skipping LXD network creation. VMs will attach to parent '%s'", nicParent)
		}
	}

	// Ensure MAAS VM profile exists in the project
	if err := ensureMAASProfile(cProj, nicType, effectiveParent, storagePoolName); err != nil {
		log.Printf("Warning: Failed to ensure MAAS profile: %v", err)
	}

	// Configure LXD to listen on the node IP only
	if err := configureLXDNetwork(trustPassword, hostIP); err != nil {
		log.Printf("Warning: Failed to configure LXD network: %v", err)
		log.Println("Continuing anyway, as this is not critical")
	}

	return nil
}

// ensureLXDProject ensures an LXD project exists
func ensureLXDProject(c lxdclient.InstanceServer, project string) error {
	if project == "" || project == "default" {
		return nil
	}
	projects, err := c.GetProjects()
	if err != nil {
		return err
	}
	for _, p := range projects {
		if p.Name == project {
			return nil
		}
	}
	req := api.ProjectsPost{
		Name: project,
		ProjectPut: api.ProjectPut{
			Config: map[string]string{},
		},
	}
	return c.CreateProject(req)
}

// configureLXDNetwork configures LXD to listen on the network
// ensureMAASProfile makes sure a profile named "maas-kvm" exists with root disk and bridged nic
func ensureMAASProfile(c lxdclient.InstanceServer, nicType, nicParent, pool string) error {
	const profileName = "maas-kvm"
	profiles, err := c.GetProfiles()
	if err != nil {
		return fmt.Errorf("list profiles: %w", err)
	}
	for _, p := range profiles {
		if p.Name == profileName {
			return nil // already present
		}
	}
	// Map network mode to device nictype expected by LXD
	deviceNictype := nicType
	if deviceNictype == "bridge" {
		deviceNictype = "bridged"
	}
	profile := api.ProfilesPost{
		Name: profileName,
		ProfilePut: api.ProfilePut{
			Description: "Profile used by MAAS for KVM VMs",
			Devices: map[string]map[string]string{
				"root": {
					"type": "disk",
					"pool": pool,
					"path": "/",
				},
				"eth0": {
					"type":    "nic",
					"nictype": deviceNictype,
					"parent":  nicParent,
					"name":    "eth0",
				},
			},
		},
	}
	if err := c.CreateProfile(profile); err != nil {
		return fmt.Errorf("create profile: %w", err)
	}
	return nil
}
func configureLXDNetwork(trustPassword, hostIP string) error {
	log.Println("Configuring LXD to listen on the network")

	// Prefer binding to the node's IP to avoid exposing on all interfaces
	address := ":8443"
	if ip := strings.TrimSpace(hostIP); ip != "" {
		address = ip + ":8443"
	}

	cmd := exec.Command("/snap/bin/lxc", "config", "set", "core.https_address", address)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("lxc command failed (%v), trying nsenter fallback", err)
		cmd = exec.Command("nsenter", "-t", "1", "-m", "-p", "--", "/snap/bin/lxc", "config", "set", "core.https_address", address)
		output, err = cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to set core.https_address (fallback): %s: %w", string(output), err)
		}
	}

	// Also set trust password (verification inside helper)
	if trustPassword != "" {
		if err := setTrustPassword(trustPassword); err != nil {
			return err
		}
	}
	// Restart LXD to apply changes (avoid systemd; use snap)
	cmd = exec.Command("nsenter", "-t", "1", "-m", "-p", "--", "snap", "restart", "lxd")
	output, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to restart LXD via snap: %s: %w", string(output), err)
	}

	log.Printf("LXD configured to listen on %s", address)
	return nil
}

// validateLXDFunctional validates that LXD is actually functional before labeling the node.
// This prevents labeling nodes where LXD initialization failed or is incomplete.
func validateLXDFunctional() error {
	socketPath, err := findLXDSocket()
	if err != nil {
		return fmt.Errorf("LXD socket not found: %w", err)
	}

	c, err := lxdclient.ConnectLXDUnix(socketPath, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to LXD: %w", err)
	}

	// Try to get server info - this validates LXD is responding
	server, _, err := c.GetServer()
	if err != nil {
		return fmt.Errorf("failed to get LXD server info: %w", err)
	}

	// Check storage pools exist
	pools, err := c.GetStoragePools()
	if err != nil {
		return fmt.Errorf("failed to get storage pools: %w", err)
	}
	if len(pools) == 0 {
		return fmt.Errorf("no storage pools found - LXD not properly initialized")
	}

	// Check networks exist
	networks, err := c.GetNetworks()
	if err != nil {
		return fmt.Errorf("failed to get networks: %w", err)
	}
	if len(networks) == 0 {
		return fmt.Errorf("no networks found - LXD not properly initialized")
	}

	log.Printf("LXD validation passed: API version %s, %d pools, %d networks",
		server.APIVersion, len(pools), len(networks))
	return nil
}
