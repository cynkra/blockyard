package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
)

// --- mock dockerClient ---

// mockDockerClient implements dockerClient. Only the methods under test need
// real implementations; the rest panic so unexpected calls surface immediately.
type mockDockerClient struct {
	containerInspectFn  func(ctx context.Context, id string) (container.InspectResponse, error)
	containerListFn     func(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	containerRemoveFn   func(ctx context.Context, containerID string, options container.RemoveOptions) error
	containerStopFn     func(ctx context.Context, containerID string, options container.StopOptions) error
	imageInspectFn      func(ctx context.Context, imageID string, opts ...client.ImageInspectOption) (image.InspectResponse, error)
	imagePullFn         func(ctx context.Context, refStr string, options image.PullOptions) (io.ReadCloser, error)
	networkConnectFn    func(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error
	networkCreateFn     func(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	networkDisconnectFn func(ctx context.Context, networkID, containerID string, force bool) error
	networkInspectFn    func(ctx context.Context, id string, opts network.InspectOptions) (network.Inspect, error)
	networkListFn       func(ctx context.Context, options network.ListOptions) ([]network.Summary, error)
	networkRemoveFn     func(ctx context.Context, networkID string) error
}

func (m *mockDockerClient) ContainerCreate(context.Context, *container.Config, *container.HostConfig, *network.NetworkingConfig, *ocispec.Platform, string) (container.CreateResponse, error) {
	panic("not implemented")
}

func (m *mockDockerClient) ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error) {
	if m.containerInspectFn != nil {
		return m.containerInspectFn(ctx, id)
	}
	panic("ContainerInspect not implemented")
}

func (m *mockDockerClient) ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error) {
	if m.containerListFn != nil {
		return m.containerListFn(ctx, options)
	}
	panic("ContainerList not implemented")
}

func (m *mockDockerClient) ContainerLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	panic("not implemented")
}

func (m *mockDockerClient) ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error {
	if m.containerRemoveFn != nil {
		return m.containerRemoveFn(ctx, containerID, options)
	}
	panic("ContainerRemove not implemented")
}

func (m *mockDockerClient) ContainerStart(context.Context, string, container.StartOptions) error {
	panic("not implemented")
}

func (m *mockDockerClient) ContainerStatsOneShot(context.Context, string) (container.StatsResponseReader, error) {
	panic("not implemented")
}

func (m *mockDockerClient) ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error {
	if m.containerStopFn != nil {
		return m.containerStopFn(ctx, containerID, options)
	}
	panic("ContainerStop not implemented")
}

func (m *mockDockerClient) ContainerWait(context.Context, string, container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	panic("not implemented")
}

func (m *mockDockerClient) ImageInspect(ctx context.Context, imageID string, opts ...client.ImageInspectOption) (image.InspectResponse, error) {
	if m.imageInspectFn != nil {
		return m.imageInspectFn(ctx, imageID, opts...)
	}
	panic("ImageInspect not implemented")
}

func (m *mockDockerClient) ImagePull(ctx context.Context, refStr string, options image.PullOptions) (io.ReadCloser, error) {
	if m.imagePullFn != nil {
		return m.imagePullFn(ctx, refStr, options)
	}
	panic("ImagePull not implemented")
}

func (m *mockDockerClient) NetworkConnect(ctx context.Context, networkID, containerID string, cfg *network.EndpointSettings) error {
	if m.networkConnectFn != nil {
		return m.networkConnectFn(ctx, networkID, containerID, cfg)
	}
	panic("NetworkConnect not implemented")
}

func (m *mockDockerClient) NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error) {
	if m.networkCreateFn != nil {
		return m.networkCreateFn(ctx, name, options)
	}
	panic("NetworkCreate not implemented")
}

func (m *mockDockerClient) NetworkDisconnect(ctx context.Context, networkID, containerID string, force bool) error {
	if m.networkDisconnectFn != nil {
		return m.networkDisconnectFn(ctx, networkID, containerID, force)
	}
	panic("NetworkDisconnect not implemented")
}

func (m *mockDockerClient) NetworkInspect(ctx context.Context, id string, opts network.InspectOptions) (network.Inspect, error) {
	if m.networkInspectFn != nil {
		return m.networkInspectFn(ctx, id, opts)
	}
	panic("NetworkInspect not implemented")
}

func (m *mockDockerClient) NetworkList(ctx context.Context, options network.ListOptions) ([]network.Summary, error) {
	if m.networkListFn != nil {
		return m.networkListFn(ctx, options)
	}
	panic("NetworkList not implemented")
}

func (m *mockDockerClient) NetworkRemove(ctx context.Context, networkID string) error {
	if m.networkRemoveFn != nil {
		return m.networkRemoveFn(ctx, networkID)
	}
	panic("NetworkRemove not implemented")
}

// --- helpers ---

func newTestBackend(cli dockerClient, opts ...func(*DockerBackend)) *DockerBackend {
	d := &DockerBackend{
		client:  cli,
		config:  &config.DockerConfig{},
		runCmd:  defaultCmdRunner,
		workers: make(map[string]*workerState),
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// --- existing unit tests ---

func TestParseMemoryLimit(t *testing.T) {
	tests := []struct {
		input string
		want  int64
		ok    bool
	}{
		{"512m", 512 * 1024 * 1024, true},
		{"1g", 1024 * 1024 * 1024, true},
		{"256mb", 256 * 1024 * 1024, true},
		{"100kb", 100 * 1024, true},
		{"1024", 1024, true},
		{"  2g  ", 2 * 1024 * 1024 * 1024, true},
		{"invalid", 0, false},
	}

	for _, tt := range tests {
		got, ok := ParseMemoryLimit(tt.input)
		if ok != tt.ok {
			t.Errorf("ParseMemoryLimit(%q) ok = %v, want %v", tt.input, ok, tt.ok)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("ParseMemoryLimit(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestExtractContainerIDFromCgroup(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{
			"0::/docker/abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
			"abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
		},
		{
			"0::/system.slice/docker-abc123def456abc123def456abc123def456abc123def456abc123def456abcd.scope",
			"abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
		},
		{"0::/user.slice/user-1000.slice", ""},
		{"0::/docker/abc", ""}, // too short
	}

	for _, tt := range tests {
		got := extractContainerIDFromCgroup(tt.line)
		if got != tt.want {
			t.Errorf("extractContainerIDFromCgroup(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
}

func TestIsHex(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"abc123", true},
		{"ABC123", true},
		{"0123456789abcdef", true},
		{"xyz", false},
		{"abc-123", false},
	}

	for _, tt := range tests {
		got := isHex(tt.input)
		if got != tt.want {
			t.Errorf("isHex(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestWorkerLabels(t *testing.T) {
	spec := backend.WorkerSpec{
		AppID:    "app-1",
		WorkerID: "worker-1",
		Labels:   map[string]string{"custom": "value"},
	}
	labels := workerLabels(spec)

	expected := map[string]string{
		"dev.blockyard/managed":   "true",
		"dev.blockyard/app-id":    "app-1",
		"dev.blockyard/worker-id": "worker-1",
		"dev.blockyard/role":      "worker",
		"custom":                  "value",
	}
	for k, v := range expected {
		if labels[k] != v {
			t.Errorf("workerLabels[%q] = %q, want %q", k, labels[k], v)
		}
	}
}

func TestBuildLabels(t *testing.T) {
	spec := backend.BuildSpec{
		AppID:    "app-1",
		BundleID: "bundle-1",
		Labels:   map[string]string{},
	}
	labels := buildLabels(spec)

	expected := map[string]string{
		"dev.blockyard/managed":   "true",
		"dev.blockyard/app-id":    "app-1",
		"dev.blockyard/bundle-id": "bundle-1",
		"dev.blockyard/role":      "build",
	}
	for k, v := range expected {
		if labels[k] != v {
			t.Errorf("buildLabels[%q] = %q, want %q", k, labels[k], v)
		}
	}
}

func TestDetectMountModeNative(t *testing.T) {
	cfg, err := detectMountMode(context.Background(), nil, "", "/data/bundles")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != MountModeNative {
		t.Errorf("expected MountModeNative, got %d", cfg.Mode)
	}
}

func TestValidIptablesComment(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"blockyard-worker-abc123", true},
		{"my_comment", true},
		{"ABC", true},
		{"a1-b2_c3", true},
		{"", false},
		{"has space", false},
		{"semi;colon", false},
		{"quo\"te", false},
		{"back`tick", false},
		{"pipe|char", false},
		{"dollar$", false},
	}

	for _, tt := range tests {
		got := validIptablesComment(tt.input)
		if got != tt.want {
			t.Errorf("validIptablesComment(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestDetectServerID_FromEnv(t *testing.T) {
	t.Setenv("BLOCKYARD_SERVER_ID", "abc123def456")
	got := detectServerID()
	if got != "abc123def456" {
		t.Errorf("detectServerID() = %q, want abc123def456", got)
	}
}

func TestNetworkLabels(t *testing.T) {
	labels := networkLabels("app-1", "worker-1")

	expected := map[string]string{
		"dev.blockyard/managed":   "true",
		"dev.blockyard/app-id":    "app-1",
		"dev.blockyard/worker-id": "worker-1",
	}
	for k, v := range expected {
		if labels[k] != v {
			t.Errorf("networkLabels[%q] = %q, want %q", k, labels[k], v)
		}
	}
}

// --- detectMountMode with mock client ---

func TestDetectMountMode_Volume(t *testing.T) {
	mock := &mockDockerClient{
		containerInspectFn: func(_ context.Context, _ string) (container.InspectResponse, error) {
			return container.InspectResponse{
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "data-vol", Destination: "/data"},
				},
			}, nil
		},
	}
	cfg, err := detectMountMode(context.Background(), mock, "server-123", "/data/bundles")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != MountModeVolume {
		t.Errorf("got mode %d, want MountModeVolume", cfg.Mode)
	}
	if cfg.VolumeName != "data-vol" {
		t.Errorf("got volume %q, want data-vol", cfg.VolumeName)
	}
	if cfg.MountDest != "/data" {
		t.Errorf("got dest %q, want /data", cfg.MountDest)
	}
}

func TestDetectMountMode_Bind(t *testing.T) {
	mock := &mockDockerClient{
		containerInspectFn: func(_ context.Context, _ string) (container.InspectResponse, error) {
			return container.InspectResponse{
				Mounts: []container.MountPoint{
					{Type: mount.TypeBind, Source: "/host/data", Destination: "/data"},
				},
			}, nil
		},
	}
	cfg, err := detectMountMode(context.Background(), mock, "server-123", "/data/bundles")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != MountModeBind {
		t.Errorf("got mode %d, want MountModeBind", cfg.Mode)
	}
	if cfg.HostSource != "/host/data" {
		t.Errorf("got source %q, want /host/data", cfg.HostSource)
	}
}

func TestDetectMountMode_LongestPrefixWins(t *testing.T) {
	mock := &mockDockerClient{
		containerInspectFn: func(_ context.Context, _ string) (container.InspectResponse, error) {
			return container.InspectResponse{
				Mounts: []container.MountPoint{
					{Type: mount.TypeBind, Source: "/host/root", Destination: "/"},
					{Type: mount.TypeVolume, Name: "data-vol", Destination: "/data"},
				},
			}, nil
		},
	}
	cfg, err := detectMountMode(context.Background(), mock, "server-123", "/data/bundles")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != MountModeVolume {
		t.Errorf("got mode %d, want MountModeVolume (longest prefix)", cfg.Mode)
	}
}

func TestDetectMountMode_NoMatchingMount(t *testing.T) {
	mock := &mockDockerClient{
		containerInspectFn: func(_ context.Context, _ string) (container.InspectResponse, error) {
			return container.InspectResponse{
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "other", Destination: "/other"},
				},
			}, nil
		},
	}
	_, err := detectMountMode(context.Background(), mock, "server-123", "/data/bundles")
	if err == nil {
		t.Fatal("expected error for no matching mount")
	}
}

func TestDetectMountMode_InspectError(t *testing.T) {
	mock := &mockDockerClient{
		containerInspectFn: func(_ context.Context, _ string) (container.InspectResponse, error) {
			return container.InspectResponse{}, errors.New("connection refused")
		},
	}
	_, err := detectMountMode(context.Background(), mock, "server-123", "/data/bundles")
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- connectServiceContainers ---

func TestConnectServiceContainers_EmptyServiceNetwork(t *testing.T) {
	d := newTestBackend(nil)
	// config.ServiceNetwork is empty by default → no-op
	if err := d.connectServiceContainers(context.Background(), "worker-net"); err != nil {
		t.Fatal(err)
	}
}

func TestConnectServiceContainers_ConnectsWithAliases(t *testing.T) {
	var connected []string

	mock := &mockDockerClient{
		networkInspectFn: func(_ context.Context, id string, _ network.InspectOptions) (network.Inspect, error) {
			return network.Inspect{
				Containers: map[string]network.EndpointResource{
					"db-container":    {},
					"redis-container": {},
				},
			}, nil
		},
		containerInspectFn: func(_ context.Context, id string) (container.InspectResponse, error) {
			aliases := map[string][]string{
				"db-container":    {"postgres", "db"},
				"redis-container": {"redis"},
			}
			return container.InspectResponse{
				NetworkSettings: &container.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"svc-net": {Aliases: aliases[id]},
					},
				},
			}, nil
		},
		networkConnectFn: func(_ context.Context, networkID, containerID string, cfg *network.EndpointSettings) error {
			connected = append(connected, containerID)
			return nil
		},
	}

	d := newTestBackend(mock)
	d.config.ServiceNetwork = "svc-net"

	if err := d.connectServiceContainers(context.Background(), "worker-net"); err != nil {
		t.Fatal(err)
	}
	if len(connected) != 2 {
		t.Fatalf("expected 2 containers connected, got %d", len(connected))
	}
}

func TestConnectServiceContainers_SkipsServerContainer(t *testing.T) {
	var connected []string

	mock := &mockDockerClient{
		networkInspectFn: func(_ context.Context, _ string, _ network.InspectOptions) (network.Inspect, error) {
			return network.Inspect{
				Containers: map[string]network.EndpointResource{
					"server-id":   {},
					"db-container": {},
				},
			}, nil
		},
		containerInspectFn: func(_ context.Context, _ string) (container.InspectResponse, error) {
			return container.InspectResponse{
				NetworkSettings: &container.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{},
				},
			}, nil
		},
		networkConnectFn: func(_ context.Context, _, containerID string, _ *network.EndpointSettings) error {
			connected = append(connected, containerID)
			return nil
		},
	}

	d := newTestBackend(mock)
	d.config.ServiceNetwork = "svc-net"
	d.serverID = "server-id"

	if err := d.connectServiceContainers(context.Background(), "worker-net"); err != nil {
		t.Fatal(err)
	}
	if len(connected) != 1 || connected[0] != "db-container" {
		t.Fatalf("expected only db-container connected, got %v", connected)
	}
}

func TestConnectServiceContainers_InspectErrorSkipsContainer(t *testing.T) {
	var connected []string

	mock := &mockDockerClient{
		networkInspectFn: func(_ context.Context, _ string, _ network.InspectOptions) (network.Inspect, error) {
			return network.Inspect{
				Containers: map[string]network.EndpointResource{
					"bad-container":  {},
					"good-container": {},
				},
			}, nil
		},
		containerInspectFn: func(_ context.Context, id string) (container.InspectResponse, error) {
			if id == "bad-container" {
				return container.InspectResponse{}, errors.New("gone")
			}
			return container.InspectResponse{
				NetworkSettings: &container.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{},
				},
			}, nil
		},
		networkConnectFn: func(_ context.Context, _, containerID string, _ *network.EndpointSettings) error {
			connected = append(connected, containerID)
			return nil
		},
	}

	d := newTestBackend(mock)
	d.config.ServiceNetwork = "svc-net"

	if err := d.connectServiceContainers(context.Background(), "worker-net"); err != nil {
		t.Fatal(err)
	}
	// bad-container skipped, good-container connected
	if len(connected) != 1 || connected[0] != "good-container" {
		t.Fatalf("expected [good-container], got %v", connected)
	}
}

func TestConnectServiceContainers_NetworkInspectError(t *testing.T) {
	mock := &mockDockerClient{
		networkInspectFn: func(_ context.Context, _ string, _ network.InspectOptions) (network.Inspect, error) {
			return network.Inspect{}, errors.New("network not found")
		},
	}

	d := newTestBackend(mock)
	d.config.ServiceNetwork = "svc-net"

	err := d.connectServiceContainers(context.Background(), "worker-net")
	if err == nil {
		t.Fatal("expected error from NetworkInspect failure")
	}
}

// --- insertMetadataRule ---

func TestInsertMetadataRule_Success(t *testing.T) {
	mock := &mockDockerClient{
		networkInspectFn: func(_ context.Context, _ string, _ network.InspectOptions) (network.Inspect, error) {
			return network.Inspect{
				IPAM: network.IPAM{
					Config: []network.IPAMConfig{{Subnet: "172.18.0.0/16"}},
				},
			}, nil
		},
	}

	var gotArgs []string
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotArgs = append([]string{name}, args...)
		return nil, nil
	}

	d := newTestBackend(mock, func(d *DockerBackend) { d.runCmd = runner })

	if err := d.insertMetadataRule(context.Background(), "test-net", "worker-1"); err != nil {
		t.Fatal(err)
	}

	expected := []string{
		"iptables", "-I", "DOCKER-USER",
		"-s", "172.18.0.0/16",
		"-d", "169.254.169.254/32",
		"-j", "DROP",
		"-m", "comment", "--comment", "blockyard-worker-1",
	}
	if len(gotArgs) != len(expected) {
		t.Fatalf("got %d args, want %d: %v", len(gotArgs), len(expected), gotArgs)
	}
	for i, want := range expected {
		if gotArgs[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, gotArgs[i], want)
		}
	}
}

func TestInsertMetadataRule_NoIPAM(t *testing.T) {
	mock := &mockDockerClient{
		networkInspectFn: func(_ context.Context, _ string, _ network.InspectOptions) (network.Inspect, error) {
			return network.Inspect{IPAM: network.IPAM{}}, nil
		},
	}
	d := newTestBackend(mock)
	err := d.insertMetadataRule(context.Background(), "test-net", "worker-1")
	if err == nil || !strings.Contains(err.Error(), "no IPAM config") {
		t.Fatalf("expected IPAM error, got %v", err)
	}
}

func TestInsertMetadataRule_NetworkInspectError(t *testing.T) {
	mock := &mockDockerClient{
		networkInspectFn: func(_ context.Context, _ string, _ network.InspectOptions) (network.Inspect, error) {
			return network.Inspect{}, errors.New("not found")
		},
	}
	d := newTestBackend(mock)
	err := d.insertMetadataRule(context.Background(), "test-net", "worker-1")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestInsertMetadataRule_IptablesFails(t *testing.T) {
	mock := &mockDockerClient{
		networkInspectFn: func(_ context.Context, _ string, _ network.InspectOptions) (network.Inspect, error) {
			return network.Inspect{
				IPAM: network.IPAM{
					Config: []network.IPAMConfig{{Subnet: "172.18.0.0/16"}},
				},
			}, nil
		},
	}
	runner := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, errors.New("permission denied")
	}
	d := newTestBackend(mock, func(d *DockerBackend) { d.runCmd = runner })

	err := d.insertMetadataRule(context.Background(), "test-net", "worker-1")
	if err == nil || !strings.Contains(err.Error(), "insert iptables rule") {
		t.Fatalf("expected iptables error, got %v", err)
	}
}

// --- blockMetadataEndpoint ---

func TestBlockMetadataEndpoint_FirstCall_IptablesSucceeds(t *testing.T) {
	mock := &mockDockerClient{
		networkInspectFn: func(_ context.Context, _ string, _ network.InspectOptions) (network.Inspect, error) {
			return network.Inspect{
				IPAM: network.IPAM{
					Config: []network.IPAMConfig{{Subnet: "172.18.0.0/16"}},
				},
			}, nil
		},
	}
	runner := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, nil // iptables succeeds
	}

	d := newTestBackend(mock, func(d *DockerBackend) { d.runCmd = runner })

	if err := d.blockMetadataEndpoint(context.Background(), "test-net", "worker-1"); err != nil {
		t.Fatal(err)
	}
	if d.metaMode != metadataBlocked {
		t.Errorf("expected metadataBlocked, got %d", d.metaMode)
	}
}

func TestBlockMetadataEndpoint_FirstCall_IptablesFailsHostBlocks(t *testing.T) {
	mock := &mockDockerClient{
		networkInspectFn: func(_ context.Context, _ string, _ network.InspectOptions) (network.Inspect, error) {
			return network.Inspect{
				IPAM: network.IPAM{
					Config: []network.IPAMConfig{{Subnet: "172.18.0.0/16"}},
				},
			}, nil
		},
	}

	callCount := 0
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		callCount++
		if callCount == 1 {
			// First call: iptables -I fails
			return nil, errors.New("permission denied")
		}
		// Second call: iptables -S DOCKER-USER succeeds with a DROP rule
		return []byte("-A DOCKER-USER -d 169.254.169.254/32 -j DROP\n"), nil
	}

	d := newTestBackend(mock, func(d *DockerBackend) { d.runCmd = runner })

	if err := d.blockMetadataEndpoint(context.Background(), "test-net", "worker-1"); err != nil {
		t.Fatal(err)
	}
	if d.metaMode != metadataHostManaged {
		t.Errorf("expected metadataHostManaged, got %d", d.metaMode)
	}
}

func TestBlockMetadataEndpoint_FirstCall_NothingWorks(t *testing.T) {
	mock := &mockDockerClient{
		networkInspectFn: func(_ context.Context, _ string, _ network.InspectOptions) (network.Inspect, error) {
			return network.Inspect{
				IPAM: network.IPAM{
					Config: []network.IPAMConfig{{Subnet: "172.18.0.0/16"}},
				},
			}, nil
		},
	}
	runner := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, errors.New("not available")
	}

	d := newTestBackend(mock, func(d *DockerBackend) { d.runCmd = runner })

	err := d.blockMetadataEndpoint(context.Background(), "test-net", "worker-1")
	if err == nil || !strings.Contains(err.Error(), "cannot block metadata endpoint") {
		t.Fatalf("expected 'cannot block' error, got %v", err)
	}
}

func TestBlockMetadataEndpoint_SubsequentCall_Blocked(t *testing.T) {
	mock := &mockDockerClient{
		networkInspectFn: func(_ context.Context, _ string, _ network.InspectOptions) (network.Inspect, error) {
			return network.Inspect{
				IPAM: network.IPAM{
					Config: []network.IPAMConfig{{Subnet: "172.18.0.0/16"}},
				},
			}, nil
		},
	}

	var iptablesCalls int
	runner := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		iptablesCalls++
		return nil, nil
	}

	d := newTestBackend(mock, func(d *DockerBackend) {
		d.runCmd = runner
		d.metaMode = metadataBlocked // simulate previous successful call
	})

	if err := d.blockMetadataEndpoint(context.Background(), "test-net", "worker-2"); err != nil {
		t.Fatal(err)
	}
	if iptablesCalls != 1 {
		t.Errorf("expected 1 iptables call (insertMetadataRule), got %d", iptablesCalls)
	}
}

func TestBlockMetadataEndpoint_SubsequentCall_HostManaged(t *testing.T) {
	d := newTestBackend(nil, func(d *DockerBackend) {
		d.metaMode = metadataHostManaged
	})

	if err := d.blockMetadataEndpoint(context.Background(), "test-net", "worker-2"); err != nil {
		t.Fatal(err)
	}
}

func TestBlockMetadataEndpoint_InvalidWorkerID(t *testing.T) {
	d := newTestBackend(nil)
	err := d.blockMetadataEndpoint(context.Background(), "test-net", "bad;id")
	if err == nil || !strings.Contains(err.Error(), "invalid worker ID") {
		t.Fatalf("expected invalid worker ID error, got %v", err)
	}
}

// --- dockerUserBlocksMetadata ---

func TestDockerUserBlocksMetadata_DropRulePresent(t *testing.T) {
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		return []byte(
			"-P DOCKER-USER ACCEPT\n" +
				"-A DOCKER-USER -d 169.254.169.254/32 -j DROP\n" +
				"-A DOCKER-USER -j RETURN\n",
		), nil
	}
	d := newTestBackend(nil, func(d *DockerBackend) { d.runCmd = runner })
	if !d.dockerUserBlocksMetadata() {
		t.Error("expected true when DROP rule is present")
	}
}

func TestDockerUserBlocksMetadata_NoDropRule(t *testing.T) {
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		return []byte("-P DOCKER-USER ACCEPT\n-A DOCKER-USER -j RETURN\n"), nil
	}
	d := newTestBackend(nil, func(d *DockerBackend) { d.runCmd = runner })
	if d.dockerUserBlocksMetadata() {
		t.Error("expected false when no DROP rule")
	}
}

func TestDockerUserBlocksMetadata_BothAttemptsFail(t *testing.T) {
	runner := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, errors.New("not available")
	}
	d := newTestBackend(nil, func(d *DockerBackend) { d.runCmd = runner })
	if d.dockerUserBlocksMetadata() {
		t.Error("expected false when all iptables attempts fail")
	}
}

// --- deleteIptablesRulesByComment ---

func TestDeleteIptablesRulesByComment_FindsAndDeletes(t *testing.T) {
	iptablesOutput := strings.Join([]string{
		"-P DOCKER-USER ACCEPT",
		`-A DOCKER-USER -s 172.18.0.0/16 -d 169.254.169.254/32 -m comment --comment "blockyard-worker-1" -j DROP`,
		"-A DOCKER-USER -j RETURN",
	}, "\n")

	var deleteCalls [][]string
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		if strings.Contains(key, "-S DOCKER-USER") {
			return []byte(iptablesOutput), nil
		}
		if strings.Contains(key, "-D DOCKER-USER") {
			deleteCalls = append(deleteCalls, args)
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected command: %s", key)
	}
	d := newTestBackend(nil, func(d *DockerBackend) { d.runCmd = runner })
	d.deleteIptablesRulesByComment("blockyard-worker-1")

	if len(deleteCalls) != 1 {
		t.Fatalf("expected 1 delete call, got %d", len(deleteCalls))
	}
	// Verify the -D command includes the subnet and comment
	deleteArgs := strings.Join(deleteCalls[0], " ")
	if !strings.Contains(deleteArgs, "172.18.0.0/16") {
		t.Errorf("delete args missing subnet: %s", deleteArgs)
	}
}

func TestDeleteIptablesRulesByComment_NoMatchingRules(t *testing.T) {
	runner := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("-P DOCKER-USER ACCEPT\n-A DOCKER-USER -j RETURN\n"), nil
	}
	d := newTestBackend(nil, func(d *DockerBackend) { d.runCmd = runner })
	// Should not panic or error
	d.deleteIptablesRulesByComment("blockyard-nonexistent")
}

func TestDeleteIptablesRulesByComment_ListFails(t *testing.T) {
	runner := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, errors.New("permission denied")
	}
	d := newTestBackend(nil, func(d *DockerBackend) { d.runCmd = runner })
	// Should return silently
	d.deleteIptablesRulesByComment("blockyard-worker-1")
}

func TestDeleteIptablesRulesByComment_MultipleRules(t *testing.T) {
	iptablesOutput := strings.Join([]string{
		`-A DOCKER-USER -s 172.18.0.0/16 -d 169.254.169.254/32 -m comment --comment "blockyard-worker-1" -j DROP`,
		`-A DOCKER-USER -s 172.19.0.0/16 -d 169.254.169.254/32 -m comment --comment "blockyard-worker-1" -j DROP`,
	}, "\n")

	var deleteCalls int
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "-S") {
			return []byte(iptablesOutput), nil
		}
		deleteCalls++
		return nil, nil
	}
	d := newTestBackend(nil, func(d *DockerBackend) { d.runCmd = runner })
	d.deleteIptablesRulesByComment("blockyard-worker-1")

	if deleteCalls != 2 {
		t.Errorf("expected 2 delete calls, got %d", deleteCalls)
	}
}

// --- cleanupOrphanMetadataRulesWithRunner ---

func TestCleanupOrphanRules_FindsAndDeletes(t *testing.T) {
	iptablesOutput := strings.Join([]string{
		"-P DOCKER-USER ACCEPT",
		`-A DOCKER-USER -s 172.18.0.0/16 -d 169.254.169.254/32 -m comment --comment "blockyard-old-worker" -j DROP`,
		"-A DOCKER-USER -j RETURN",
	}, "\n")

	var deleteCalls int
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "-S") {
			return []byte(iptablesOutput), nil
		}
		deleteCalls++
		return nil, nil
	}
	cleanupOrphanMetadataRulesWithRunner(context.Background(), runner)

	if deleteCalls != 1 {
		t.Errorf("expected 1 delete call, got %d", deleteCalls)
	}
}

func TestCleanupOrphanRules_NoOrphans(t *testing.T) {
	runner := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("-P DOCKER-USER ACCEPT\n-A DOCKER-USER -j RETURN\n"), nil
	}
	// Should not panic
	cleanupOrphanMetadataRulesWithRunner(context.Background(), runner)
}

// --- ensureImage ---

func TestEnsureImage_AlreadyPresent(t *testing.T) {
	mock := &mockDockerClient{
		imageInspectFn: func(_ context.Context, _ string, _ ...client.ImageInspectOption) (image.InspectResponse, error) {
			return image.InspectResponse{}, nil
		},
	}
	d := newTestBackend(mock)
	if err := d.ensureImage(context.Background(), "alpine:3.21"); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureImage_PullsWhenMissing(t *testing.T) {
	var pulled bool
	mock := &mockDockerClient{
		imageInspectFn: func(_ context.Context, _ string, _ ...client.ImageInspectOption) (image.InspectResponse, error) {
			return image.InspectResponse{}, errors.New("not found")
		},
		imagePullFn: func(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
			pulled = true
			if ref != "alpine:3.21" {
				t.Errorf("pulled %q, want alpine:3.21", ref)
			}
			return io.NopCloser(strings.NewReader("")), nil
		},
	}
	d := newTestBackend(mock)
	if err := d.ensureImage(context.Background(), "alpine:3.21"); err != nil {
		t.Fatal(err)
	}
	if !pulled {
		t.Error("expected ImagePull to be called")
	}
}

func TestEnsureImage_PullFails(t *testing.T) {
	mock := &mockDockerClient{
		imageInspectFn: func(_ context.Context, _ string, _ ...client.ImageInspectOption) (image.InspectResponse, error) {
			return image.InspectResponse{}, errors.New("not found")
		},
		imagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return nil, errors.New("registry unavailable")
		},
	}
	d := newTestBackend(mock)
	err := d.ensureImage(context.Background(), "alpine:3.21")
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- disconnectServiceContainers ---

func TestDisconnectServiceContainers_EmptyServiceNetwork(t *testing.T) {
	d := newTestBackend(nil)
	// config.ServiceNetwork is empty by default → no-op
	d.disconnectServiceContainers(context.Background(), "worker-net")
}

func TestDisconnectServiceContainers_DisconnectsExceptServer(t *testing.T) {
	var disconnected []string

	mock := &mockDockerClient{
		networkInspectFn: func(_ context.Context, _ string, _ network.InspectOptions) (network.Inspect, error) {
			return network.Inspect{
				Containers: map[string]network.EndpointResource{
					"server-id":    {},
					"db-container": {},
				},
			}, nil
		},
		networkDisconnectFn: func(_ context.Context, _, containerID string, _ bool) error {
			disconnected = append(disconnected, containerID)
			return nil
		},
	}

	d := newTestBackend(mock)
	d.config.ServiceNetwork = "svc-net"
	d.serverID = "server-id"

	d.disconnectServiceContainers(context.Background(), "worker-net")
	if len(disconnected) != 1 || disconnected[0] != "db-container" {
		t.Fatalf("expected [db-container], got %v", disconnected)
	}
}

func TestDisconnectServiceContainers_NetworkInspectError(t *testing.T) {
	mock := &mockDockerClient{
		networkInspectFn: func(_ context.Context, _ string, _ network.InspectOptions) (network.Inspect, error) {
			return network.Inspect{}, errors.New("not found")
		},
	}

	d := newTestBackend(mock)
	d.config.ServiceNetwork = "svc-net"

	// Should not panic — errors are logged, not returned
	d.disconnectServiceContainers(context.Background(), "worker-net")
}

// --- createNetwork ---

func TestCreateNetwork_HappyPath(t *testing.T) {
	var createdName string
	mock := &mockDockerClient{
		networkCreateFn: func(_ context.Context, name string, opts network.CreateOptions) (network.CreateResponse, error) {
			createdName = name
			if opts.Driver != "bridge" {
				t.Errorf("expected bridge driver, got %q", opts.Driver)
			}
			return network.CreateResponse{ID: "net-new-id"}, nil
		},
	}

	d := newTestBackend(mock)
	id, err := d.createNetwork(context.Background(), "blockyard-w1", "app-1", "w1")
	if err != nil {
		t.Fatal(err)
	}
	if id != "net-new-id" {
		t.Errorf("got ID %q, want net-new-id", id)
	}
	if createdName != "blockyard-w1" {
		t.Errorf("created network %q, want blockyard-w1", createdName)
	}
}

func TestCreateNetwork_Error(t *testing.T) {
	mock := &mockDockerClient{
		networkCreateFn: func(_ context.Context, _ string, _ network.CreateOptions) (network.CreateResponse, error) {
			return network.CreateResponse{}, errors.New("quota exceeded")
		},
	}

	d := newTestBackend(mock)
	_, err := d.createNetwork(context.Background(), "blockyard-w1", "app-1", "w1")
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- Addr ---

func TestAddr_HappyPath(t *testing.T) {
	mock := &mockDockerClient{
		containerInspectFn: func(_ context.Context, _ string) (container.InspectResponse, error) {
			return container.InspectResponse{
				NetworkSettings: &container.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"blockyard-w1": {IPAddress: "172.18.0.2"},
					},
				},
			}, nil
		},
	}

	d := newTestBackend(mock)
	d.config.ShinyPort = 8080
	d.workers["w1"] = &workerState{
		containerID: "ctr-123",
		networkName: "blockyard-w1",
	}

	addr, err := d.Addr(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	if addr != "172.18.0.2:8080" {
		t.Errorf("got %q, want 172.18.0.2:8080", addr)
	}
}

func TestAddr_UnknownWorker(t *testing.T) {
	d := newTestBackend(nil)
	_, err := d.Addr(context.Background(), "nonexistent")
	if err == nil || !strings.Contains(err.Error(), "unknown worker") {
		t.Fatalf("expected unknown worker error, got %v", err)
	}
}

func TestAddr_InspectError(t *testing.T) {
	mock := &mockDockerClient{
		containerInspectFn: func(_ context.Context, _ string) (container.InspectResponse, error) {
			return container.InspectResponse{}, errors.New("gone")
		},
	}

	d := newTestBackend(mock)
	d.workers["w1"] = &workerState{containerID: "ctr-123", networkName: "net-1"}

	_, err := d.Addr(context.Background(), "w1")
	if err == nil || !strings.Contains(err.Error(), "inspect container") {
		t.Fatalf("expected inspect error, got %v", err)
	}
}

func TestAddr_NoNetworkSettings(t *testing.T) {
	mock := &mockDockerClient{
		containerInspectFn: func(_ context.Context, _ string) (container.InspectResponse, error) {
			return container.InspectResponse{NetworkSettings: nil}, nil
		},
	}

	d := newTestBackend(mock)
	d.workers["w1"] = &workerState{containerID: "ctr-123", networkName: "net-1"}

	_, err := d.Addr(context.Background(), "w1")
	if err == nil || !strings.Contains(err.Error(), "no networks") {
		t.Fatalf("expected no networks error, got %v", err)
	}
}

func TestAddr_NotOnExpectedNetwork(t *testing.T) {
	mock := &mockDockerClient{
		containerInspectFn: func(_ context.Context, _ string) (container.InspectResponse, error) {
			return container.InspectResponse{
				NetworkSettings: &container.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"other-net": {IPAddress: "172.18.0.2"},
					},
				},
			}, nil
		},
	}

	d := newTestBackend(mock)
	d.workers["w1"] = &workerState{containerID: "ctr-123", networkName: "blockyard-w1"}

	_, err := d.Addr(context.Background(), "w1")
	if err == nil || !strings.Contains(err.Error(), "not on network") {
		t.Fatalf("expected not on network error, got %v", err)
	}
}

func TestAddr_EmptyIP(t *testing.T) {
	mock := &mockDockerClient{
		containerInspectFn: func(_ context.Context, _ string) (container.InspectResponse, error) {
			return container.InspectResponse{
				NetworkSettings: &container.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"blockyard-w1": {IPAddress: ""},
					},
				},
			}, nil
		},
	}

	d := newTestBackend(mock)
	d.workers["w1"] = &workerState{containerID: "ctr-123", networkName: "blockyard-w1"}

	_, err := d.Addr(context.Background(), "w1")
	if err == nil || !strings.Contains(err.Error(), "no IP") {
		t.Fatalf("expected no IP error, got %v", err)
	}
}

// --- Stop ---

func TestStop_HappyPath(t *testing.T) {
	var stopped, removed, netRemoved bool

	mock := &mockDockerClient{
		containerStopFn: func(_ context.Context, _ string, _ container.StopOptions) error {
			stopped = true
			return nil
		},
		containerRemoveFn: func(_ context.Context, _ string, _ container.RemoveOptions) error {
			removed = true
			return nil
		},
		networkRemoveFn: func(_ context.Context, _ string) error {
			netRemoved = true
			return nil
		},
	}

	d := newTestBackend(mock, func(d *DockerBackend) {
		d.metaMode = metadataHostManaged
	})
	d.workers["w1"] = &workerState{
		containerID: "ctr-123",
		networkID:   "net-id-123",
		networkName: "blockyard-w1",
	}

	if err := d.Stop(context.Background(), "w1"); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Error("ContainerStop not called")
	}
	if !removed {
		t.Error("ContainerRemove not called")
	}
	if !netRemoved {
		t.Error("NetworkRemove not called")
	}

	// Worker should be removed from map
	d.mu.Lock()
	_, ok := d.workers["w1"]
	d.mu.Unlock()
	if ok {
		t.Error("worker should be removed from map after Stop")
	}
}

func TestStop_UnknownWorker(t *testing.T) {
	d := newTestBackend(nil)
	err := d.Stop(context.Background(), "nonexistent")
	if err == nil || !strings.Contains(err.Error(), "unknown worker") {
		t.Fatalf("expected unknown worker error, got %v", err)
	}
}

func TestStop_ContainerStopErrorStillCleansUp(t *testing.T) {
	var removeCalled, netRemoveCalled bool

	mock := &mockDockerClient{
		containerStopFn: func(_ context.Context, _ string, _ container.StopOptions) error {
			return errors.New("timeout")
		},
		containerRemoveFn: func(_ context.Context, _ string, _ container.RemoveOptions) error {
			removeCalled = true
			return nil
		},
		networkRemoveFn: func(_ context.Context, _ string) error {
			netRemoveCalled = true
			return nil
		},
	}

	d := newTestBackend(mock, func(d *DockerBackend) {
		d.metaMode = metadataHostManaged
	})
	d.workers["w1"] = &workerState{
		containerID: "ctr-123",
		networkID:   "net-id-123",
		networkName: "blockyard-w1",
	}

	err := d.Stop(context.Background(), "w1")
	if err == nil || !strings.Contains(err.Error(), "stop container") {
		t.Fatalf("expected stop container error, got %v", err)
	}
	if !removeCalled {
		t.Error("ContainerRemove should still be called after stop error")
	}
	if !netRemoveCalled {
		t.Error("NetworkRemove should still be called after stop error")
	}
}

func TestStop_DisconnectsServer(t *testing.T) {
	var serverDisconnected bool

	mock := &mockDockerClient{
		containerStopFn: func(_ context.Context, _ string, _ container.StopOptions) error {
			return nil
		},
		containerRemoveFn: func(_ context.Context, _ string, _ container.RemoveOptions) error {
			return nil
		},
		networkDisconnectFn: func(_ context.Context, _, containerID string, _ bool) error {
			if containerID == "server-id" {
				serverDisconnected = true
			}
			return nil
		},
		networkRemoveFn: func(_ context.Context, _ string) error {
			return nil
		},
	}

	d := newTestBackend(mock, func(d *DockerBackend) {
		d.metaMode = metadataHostManaged
		d.serverID = "server-id"
	})
	d.workers["w1"] = &workerState{
		containerID: "ctr-123",
		networkID:   "net-id-123",
		networkName: "blockyard-w1",
	}

	if err := d.Stop(context.Background(), "w1"); err != nil {
		t.Fatal(err)
	}
	if !serverDisconnected {
		t.Error("server should be disconnected from worker network")
	}
}

// --- ListManaged ---

func TestListManaged_ReturnsContainersAndNetworks(t *testing.T) {
	mock := &mockDockerClient{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]container.Summary, error) {
			return []container.Summary{
				{ID: "ctr-1", Labels: map[string]string{"dev.blockyard/managed": "true"}},
			}, nil
		},
		networkListFn: func(_ context.Context, _ network.ListOptions) ([]network.Summary, error) {
			return []network.Summary{
				{ID: "net-1", Labels: map[string]string{"dev.blockyard/managed": "true"}},
			}, nil
		},
	}

	d := newTestBackend(mock)
	resources, err := d.ListManaged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(resources))
	}
	// Containers should come first (sorted by Kind)
	if resources[0].Kind != backend.ResourceContainer {
		t.Error("expected container first in sorted output")
	}
	if resources[1].Kind != backend.ResourceNetwork {
		t.Error("expected network second in sorted output")
	}
}

func TestListManaged_ContainerListError(t *testing.T) {
	mock := &mockDockerClient{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]container.Summary, error) {
			return nil, errors.New("docker down")
		},
	}

	d := newTestBackend(mock)
	_, err := d.ListManaged(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestListManaged_NetworkListError(t *testing.T) {
	mock := &mockDockerClient{
		containerListFn: func(_ context.Context, _ container.ListOptions) ([]container.Summary, error) {
			return nil, nil
		},
		networkListFn: func(_ context.Context, _ network.ListOptions) ([]network.Summary, error) {
			return nil, errors.New("docker down")
		},
	}

	d := newTestBackend(mock)
	_, err := d.ListManaged(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- RemoveResource ---

func TestRemoveResource_Container(t *testing.T) {
	var removedID string
	mock := &mockDockerClient{
		containerRemoveFn: func(_ context.Context, id string, _ container.RemoveOptions) error {
			removedID = id
			return nil
		},
	}

	d := newTestBackend(mock)
	err := d.RemoveResource(context.Background(), backend.ManagedResource{
		ID:   "ctr-123",
		Kind: backend.ResourceContainer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if removedID != "ctr-123" {
		t.Errorf("removed %q, want ctr-123", removedID)
	}
}

func TestRemoveResource_Network(t *testing.T) {
	var removedID string
	mock := &mockDockerClient{
		networkRemoveFn: func(_ context.Context, id string) error {
			removedID = id
			return nil
		},
	}

	d := newTestBackend(mock)
	err := d.RemoveResource(context.Background(), backend.ManagedResource{
		ID:   "net-123",
		Kind: backend.ResourceNetwork,
	})
	if err != nil {
		t.Fatal(err)
	}
	if removedID != "net-123" {
		t.Errorf("removed %q, want net-123", removedID)
	}
}

func TestRemoveResource_UnknownKind(t *testing.T) {
	d := newTestBackend(nil)
	err := d.RemoveResource(context.Background(), backend.ManagedResource{
		ID:   "x",
		Kind: 99,
	})
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
}
