//go:build !minimal || docker_backend

package orchestrator

import (
	"context"
	"io"
	"iter"
	"net/netip"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// --- mock Docker client ---

type mockDocker struct {
	inspectFn func(ctx context.Context, id string, opts client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	createFn  func(ctx context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	startFn   func(ctx context.Context, id string, opts client.ContainerStartOptions) (client.ContainerStartResult, error)
	stopFn    func(ctx context.Context, id string, opts client.ContainerStopOptions) (client.ContainerStopResult, error)
	removeFn  func(ctx context.Context, id string, opts client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	waitFn    func(ctx context.Context, id string, opts client.ContainerWaitOptions) client.ContainerWaitResult
	pullFn    func(ctx context.Context, ref string, opts client.ImagePullOptions) (client.ImagePullResponse, error)
}

func (m *mockDocker) ContainerInspect(ctx context.Context, id string, opts client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if m.inspectFn != nil {
		return m.inspectFn(ctx, id, opts)
	}
	return defaultInspectResult(), nil
}

func (m *mockDocker) ContainerCreate(ctx context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	if m.createFn != nil {
		return m.createFn(ctx, opts)
	}
	return client.ContainerCreateResult{ID: "new-container-123"}, nil
}

func (m *mockDocker) ContainerStart(ctx context.Context, id string, opts client.ContainerStartOptions) (client.ContainerStartResult, error) {
	if m.startFn != nil {
		return m.startFn(ctx, id, opts)
	}
	return client.ContainerStartResult{}, nil
}

func (m *mockDocker) ContainerStop(ctx context.Context, id string, opts client.ContainerStopOptions) (client.ContainerStopResult, error) {
	if m.stopFn != nil {
		return m.stopFn(ctx, id, opts)
	}
	return client.ContainerStopResult{}, nil
}

func (m *mockDocker) ContainerRemove(ctx context.Context, id string, opts client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	if m.removeFn != nil {
		return m.removeFn(ctx, id, opts)
	}
	return client.ContainerRemoveResult{}, nil
}

func (m *mockDocker) ContainerWait(ctx context.Context, id string, opts client.ContainerWaitOptions) client.ContainerWaitResult {
	if m.waitFn != nil {
		return m.waitFn(ctx, id, opts)
	}
	ch := make(chan container.WaitResponse, 1)
	ch <- container.WaitResponse{}
	return client.ContainerWaitResult{Result: ch}
}

func (m *mockDocker) ImagePull(_ context.Context, _ string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	if m.pullFn != nil {
		return m.pullFn(context.Background(), "", client.ImagePullOptions{})
	}
	return mockPullResponse{ReadCloser: io.NopCloser(&emptyReader{})}, nil
}

type mockPullResponse struct {
	io.ReadCloser
}

func (m mockPullResponse) JSONMessages(_ context.Context) iter.Seq2[jsonstream.Message, error] {
	return nil
}

func (m mockPullResponse) Wait(_ context.Context) error {
	return nil
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

// --- helpers ---

func defaultInspectResult() client.ContainerInspectResult {
	return client.ContainerInspectResult{
		Container: container.InspectResponse{
			Config: &container.Config{
				Image: "ghcr.io/cynkra/blockyard:1.0.0",
				Env:   []string{"FOO=bar"},
			},
			HostConfig: &container.HostConfig{},
			NetworkSettings: &container.NetworkSettings{
				Networks: map[string]*network.EndpointSettings{
					"bridge": {
						IPAddress: netip.MustParseAddr("172.17.0.2"),
					},
				},
			},
		},
	}
}

func mustParsePort(s string) network.Port {
	p, err := network.ParsePort(s)
	if err != nil {
		panic(err)
	}
	return p
}

// ---------------------------------------------------------------------------
// Clone config
// ---------------------------------------------------------------------------

func TestCloneConfig(t *testing.T) {
	docker := &mockDocker{
		inspectFn: func(_ context.Context, _ string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{
				Container: container.InspectResponse{
					Config: &container.Config{
						Image:  "ghcr.io/cynkra/blockyard:1.0.0",
						Env:    []string{"FOO=bar", "BAZ=qux"},
						Labels: map[string]string{"app": "blockyard"},
					},
					HostConfig: &container.HostConfig{
						PortBindings: network.PortMap{
							mustParsePort("8080/tcp"): []network.PortBinding{{HostPort: "8080"}},
						},
					},
					NetworkSettings: &container.NetworkSettings{
						Networks: map[string]*network.EndpointSettings{
							"mynet": {IPAddress: netip.MustParseAddr("10.0.0.1")},
						},
					},
				},
			}, nil
		},
	}
	f := newDockerFactoryForTest(docker, "self-container-id", func() string { return "8080" })

	opts, err := f.cloneConfig(context.Background(), "ghcr.io/cynkra/blockyard:2.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}

	if opts.Config.Image != "ghcr.io/cynkra/blockyard:2.0.0" {
		t.Errorf("image = %q, want updated", opts.Config.Image)
	}

	if opts.HostConfig.PortBindings != nil {
		t.Error("port bindings should be stripped")
	}

	if opts.Config.Labels["app"] != "blockyard" {
		t.Error("labels should be preserved")
	}

	found := false
	for _, e := range opts.Config.Env {
		if e == "BLOCKYARD_PASSIVE=1" {
			found = true
		}
	}
	if !found {
		t.Error("expected BLOCKYARD_PASSIVE=1 in env")
	}

	if opts.NetworkingConfig == nil {
		t.Fatal("networking config should be set")
	}
	if _, ok := opts.NetworkingConfig.EndpointsConfig["mynet"]; !ok {
		t.Error("expected mynet in endpoints config")
	}
}

func TestCloneConfigInspectError(t *testing.T) {
	docker := &mockDocker{
		inspectFn: func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{}, io.ErrUnexpectedEOF
		},
	}
	f := newDockerFactoryForTest(docker, "self-container-id", func() string { return "8080" })

	_, err := f.cloneConfig(context.Background(), "img:new", nil)
	if err == nil {
		t.Error("expected error when inspect fails")
	}
}

func TestCloneConfigExtraEnvMalformed(t *testing.T) {
	f := newDockerFactoryForTest(&mockDocker{}, "self-container-id", func() string { return "8080" })
	opts, err := f.cloneConfig(context.Background(), "img:new", []string{"NOEQUALS"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range opts.Config.Env {
		if e == "NOEQUALS" {
			t.Error("malformed env entry should not appear verbatim")
		}
	}
}

func TestCloneConfigNilNetworkSettings(t *testing.T) {
	docker := &mockDocker{
		inspectFn: func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{Container: container.InspectResponse{
				Config:   &container.Config{Image: "old:1.0", Env: []string{}},
				HostConfig: &container.HostConfig{},
			}}, nil
		},
	}
	f := newDockerFactoryForTest(docker, "self-container-id", func() string { return "8080" })
	opts, err := f.cloneConfig(context.Background(), "img:new", nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.NetworkingConfig != nil {
		t.Error("expected nil NetworkingConfig when source has no networks")
	}
}

// ---------------------------------------------------------------------------
// Image base and tag
// ---------------------------------------------------------------------------

func TestCurrentImageBaseAndTag(t *testing.T) {
	docker := &mockDocker{
		inspectFn: func(_ context.Context, _ string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{
				Container: container.InspectResponse{
					Config:     &container.Config{Image: "ghcr.io/cynkra/blockyard:3.2.1"},
					HostConfig: &container.HostConfig{},
				},
			}, nil
		},
	}
	f := newDockerFactoryForTest(docker, "self-container-id", func() string { return "8080" })

	base := f.CurrentImageBase(context.Background())
	if base != "ghcr.io/cynkra/blockyard" {
		t.Errorf("CurrentImageBase = %q", base)
	}

	tag := f.CurrentImageTag(context.Background())
	if tag != "3.2.1" {
		t.Errorf("CurrentImageTag = %q", tag)
	}
}

func TestCurrentImageBaseNoTag(t *testing.T) {
	docker := &mockDocker{
		inspectFn: func(_ context.Context, _ string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{
				Container: container.InspectResponse{
					Config:     &container.Config{Image: "blockyard"},
					HostConfig: &container.HostConfig{},
				},
			}, nil
		},
	}
	f := newDockerFactoryForTest(docker, "self-container-id", func() string { return "8080" })

	base := f.CurrentImageBase(context.Background())
	if base != "blockyard" {
		t.Errorf("CurrentImageBase without tag = %q", base)
	}
}

func TestCurrentImageTagError(t *testing.T) {
	docker := &mockDocker{
		inspectFn: func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{}, io.ErrUnexpectedEOF
		},
	}
	f := newDockerFactoryForTest(docker, "self-container-id", func() string { return "8080" })

	tag := f.CurrentImageTag(context.Background())
	if tag != "1.0.0" {
		t.Errorf("expected fallback to version, got %q", tag)
	}

	base := f.CurrentImageBase(context.Background())
	if base != "blockyard" {
		t.Errorf("expected fallback base, got %q", base)
	}
}

// ---------------------------------------------------------------------------
// Container address
// ---------------------------------------------------------------------------

func TestContainerAddrNoNetworks(t *testing.T) {
	docker := &mockDocker{
		inspectFn: func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{
				Container: container.InspectResponse{
					Config:          &container.Config{},
					HostConfig:      &container.HostConfig{},
					NetworkSettings: &container.NetworkSettings{},
				},
			}, nil
		},
	}
	f := newDockerFactoryForTest(docker, "self-container-id", func() string { return "8080" })
	_, err := f.containerAddr(context.Background(), "some-id-12345678")
	if err == nil {
		t.Error("expected error when no networks")
	}
}

func TestContainerAddrInspectError(t *testing.T) {
	docker := &mockDocker{
		inspectFn: func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{}, io.ErrUnexpectedEOF
		},
	}
	f := newDockerFactoryForTest(docker, "self-container-id", func() string { return "8080" })
	_, err := f.containerAddr(context.Background(), "some-id-12345678")
	if err == nil {
		t.Error("expected error when inspect fails")
	}
}

// ---------------------------------------------------------------------------
// CreateInstance
// ---------------------------------------------------------------------------

func TestCreateInstanceSuccess(t *testing.T) {
	f := newDockerFactoryForTest(&mockDocker{}, "self-container-id", func() string { return "8080" })
	sender := newSender(t)

	inst, err := f.CreateInstance(context.Background(), "ghcr.io/cynkra/blockyard:2.0.0", nil, sender)
	if err != nil {
		t.Fatal(err)
	}
	if inst.ID() != "new-container-123" {
		t.Errorf("ID = %q, want new-container-123", inst.ID())
	}
}

func TestCreateInstanceStartFails(t *testing.T) {
	docker := &mockDocker{
		startFn: func(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
			return client.ContainerStartResult{}, io.ErrUnexpectedEOF
		},
	}
	f := newDockerFactoryForTest(docker, "self-container-id", func() string { return "8080" })
	sender := newSender(t)

	_, err := f.CreateInstance(context.Background(), "img:2.0", nil, sender)
	if err == nil {
		t.Error("expected error when start fails")
	}
}

// ---------------------------------------------------------------------------
// killAndRemove
// ---------------------------------------------------------------------------

func TestKillAndRemoveBestEffort(t *testing.T) {
	docker := &mockDocker{
		stopFn: func(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
			return client.ContainerStopResult{}, io.ErrUnexpectedEOF
		},
		removeFn: func(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
			return client.ContainerRemoveResult{}, io.ErrUnexpectedEOF
		},
	}
	f := newDockerFactoryForTest(docker, "self-container-id", func() string { return "8080" })

	// Should not panic or return error.
	f.killAndRemove(context.Background(), "some-container-id-1234")
}

// ---------------------------------------------------------------------------
// appendOrReplace
// ---------------------------------------------------------------------------

func TestAppendOrReplace(t *testing.T) {
	env := []string{"A=1", "B=2", "C=3"}

	env = appendOrReplace(env, "B", "99")
	if env[1] != "B=99" {
		t.Errorf("expected B=99, got %s", env[1])
	}

	env = appendOrReplace(env, "D", "4")
	if len(env) != 4 {
		t.Errorf("expected 4 entries, got %d", len(env))
	}
	if env[3] != "D=4" {
		t.Errorf("expected D=4, got %s", env[3])
	}
}
