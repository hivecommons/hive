//go:build extwork_omp

package main

import (
	"context"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/extwork/omp"
)

type dashboardOMPLink struct {
	peer dashboard.ExternalExecutionPeer
}

func (l dashboardOMPLink) Send(ctx context.Context, msg omp.Message) error {
	out := dashboard.ExternalExecutionMessage{
		Type:             msg.Type,
		Seq:              msg.Seq,
		ContributorID:    msg.ContributorID,
		WorkbenchVersion: msg.WorkbenchVersion,
		Incarnation:      msg.Incarnation,
		ExecutionKey:     msg.ExecutionKey,
		TaskID:           msg.TaskID,
		TaskGen:          msg.TaskGen,
		WorkKey:          msg.WorkKey,
		Stage:            msg.Stage,
		Summary:          msg.Summary,
		Reason:           msg.Reason,
		State:            msg.State,
		Detail:           msg.Detail,
		RemoteRunID:      msg.RemoteRunID,
		Deduplicated:     msg.Deduplicated,
		Acknowledged:     msg.Acknowledged,
		Stopped:          msg.Stopped,
		Payload:          msg.Payload,
	}
	if msg.Capabilities != nil {
		out.RelayCapabilities = append([]string(nil), msg.Capabilities.RelayCapabilities...)
	}
	if msg.Artifact != nil {
		out.Artifact = &dashboard.ExternalExecutionArtifact{
			Path:   msg.Artifact.Path,
			Digest: msg.Artifact.Digest,
			Size:   msg.Artifact.Size,
			Body:   append([]byte(nil), msg.Artifact.Body...),
		}
	}
	return l.peer.SendExternal(ctx, out)
}

func (l dashboardOMPLink) Recv(ctx context.Context) (omp.Message, error) {
	msg, err := l.peer.RecvExternal(ctx)
	if err != nil {
		return omp.Message{}, err
	}
	out := omp.Message{
		Type:             msg.Type,
		Seq:              msg.Seq,
		ContributorID:    msg.ContributorID,
		WorkbenchVersion: msg.WorkbenchVersion,
		Incarnation:      msg.Incarnation,
		ExecutionKey:     msg.ExecutionKey,
		TaskID:           msg.TaskID,
		TaskGen:          msg.TaskGen,
		WorkKey:          msg.WorkKey,
		Stage:            msg.Stage,
		Summary:          msg.Summary,
		Reason:           msg.Reason,
		State:            msg.State,
		Detail:           msg.Detail,
		RemoteRunID:      msg.RemoteRunID,
		Deduplicated:     msg.Deduplicated,
		Acknowledged:     msg.Acknowledged,
		Stopped:          msg.Stopped,
		Payload:          msg.Payload,
	}
	if len(msg.RelayCapabilities) > 0 {
		out.Capabilities = &omp.Capabilities{RelayCapabilities: append([]string(nil), msg.RelayCapabilities...)}
	}
	if msg.Artifact != nil {
		out.Artifact = &omp.Artifact{
			Path:   msg.Artifact.Path,
			Digest: msg.Artifact.Digest,
			Size:   msg.Artifact.Size,
			Body:   append([]byte(nil), msg.Artifact.Body...),
		}
	}
	return out, nil
}

func (l dashboardOMPLink) Close() error { return l.peer.CloseExternal() }

func attachExternalOMPPeer(ctx context.Context, peer dashboard.ExternalExecutionPeer) error {
	if peer == nil {
		return nil
	}
	caps := omp.Capabilities{RelayCapabilities: peer.ExternalCapabilities()}
	_, err := omp.DefaultBroker.Attach(ctx, peer.Identity(), peer.Incarnation(), peer.WorkbenchVersion(), caps, dashboardOMPLink{peer: peer})
	return err
}
