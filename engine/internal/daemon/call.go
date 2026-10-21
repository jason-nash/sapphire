package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"sapphire/engine/internal/dockerapi"
	"sapphire/engine/internal/forward"
	"sapphire/engine/internal/protocol"
)

func (d *Daemon) call(ctx context.Context, request protocol.Request) protocol.Response {
	guestAction := strings.HasPrefix(request.Action, "guest.")
	if guestAction {
		request.Action = strings.TrimPrefix(request.Action, "guest.")
	}
	hostAction := request.Action
	if guestAction {
		hostAction = ""
	}
	switch hostAction {
	case "status", "host.status":
		return protocol.Success(d.Status())
	case "host.forward.list":
		return protocol.Success(d.forwards.List())
	case "host.stop":
		return protocol.Success(d.Status())
	}
	if !d.Status().Running {
		return protocol.Failure(errors.New("guest is not ready"))
	}
	switch hostAction {
	case "docker.inventory":
		inventory, err := dockerapi.Read(ctx, d.config.Path("docker.sock"))
		if err != nil {
			return protocol.Failure(err)
		}
		return protocol.Success(inventory)
	case "host.forward", "host.forward.remove":
		var spec forward.Spec
		payload := request.Configuration
		if len(payload) == 0 || bytes.Equal(payload, []byte("null")) {
			payload = request.Arguments
		}
		if err := protocol.Decode(bytes.NewReader(payload), &spec); err != nil {
			return protocol.Failure(fmt.Errorf("forward configuration: %w", err))
		}
		if request.Action == "host.forward.remove" {
			if spec.HostPort < 1 || spec.HostPort > 65535 {
				return protocol.Failure(errors.New("hostPort must be between 1 and 65535"))
			}
			if err := d.forwards.Remove(spec.HostPort, "manual"); err != nil {
				return protocol.Failure(err)
			}
			return protocol.Success(nil)
		}
		if spec.Host == "" {
			spec.Host = "127.0.0.1"
		}
		if spec.HostPort == 0 {
			spec.HostPort = spec.Port
		}
		spec.Source = "manual"
		created, err := d.forwards.Add(spec)
		if err != nil {
			return protocol.Failure(err)
		}
		return protocol.Success(created)
	}
	switch request.Action {
	case "docker.start", "docker.stop", "k8s.start", "k8s.stop":
		d.actions.Lock()
		defer d.actions.Unlock()
	}
	var apiForwardCreated bool
	if request.Action == "k8s.start" {
		var err error
		_, apiForwardCreated, err = d.forwards.Ensure(forward.Spec{Host: "127.0.0.1", Port: 6443, HostPort: 6443, Source: "k8s"})
		if err != nil {
			return protocol.Failure(fmt.Errorf("Kubernetes API forward: %w", err))
		}
	}
	if !guestAction {
		switch request.Action {
		case "machine.list", "machine.create", "machine.start", "machine.stop",
			"machine.delete", "machine.clone", "machine.exec",
			"docker.start", "docker.stop", "docker.status", "k8s.start", "k8s.stop", "k8s.status",
			"k8s.kubeconfig", "metrics":
		default:
			return protocol.Failure(fmt.Errorf("unsupported action %q", request.Action))
		}
	}
	response, err := d.guest.Call(ctx, request)
	if err != nil {
		response = protocol.Failure(err)
	}
	if !response.OK {
		if apiForwardCreated {
			d.forwards.Remove(6443, "k8s")
		}
		return response
	}
	switch request.Action {
	case "docker.start", "k8s.start":
		d.docker.Start(d.ctx)
	case "docker.stop":
		d.docker.Stop()
	case "k8s.stop":
		if err := d.forwards.Remove(6443, "k8s"); err != nil {
			return protocol.Failure(err)
		}
	}
	return response
}

// DecodeRequest is shared by the CLI so validation errors also use the envelope.
func DecodeRequest(data string) (protocol.Request, error) {
	var request protocol.Request
	if err := protocol.Decode(strings.NewReader(data), &request); err != nil {
		return request, err
	}
	return request, request.Validate()
}
