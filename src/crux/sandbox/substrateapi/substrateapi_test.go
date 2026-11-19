package substrateapi

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// The generated code is a wire-compatible subset of Substrate's Control API.
// These tests exercise every message's accessors — empty and populated — and
// every RPC through a real gRPC connection so a regeneration that drops or
// renames a field or method fails here rather than against a cluster.

// allFiles is a function because the File_* descriptors are populated by
// the generated init functions, after package-level vars.
func allFiles() []protoreflect.FileDescriptor {
	return []protoreflect.FileDescriptor{
		File_common_proto, File_actor_proto, File_actor_rpc_proto, File_container_proto,
		File_template_proto, File_egress_proto, File_worker_proto, File_control_proto,
	}
}

// callZeroArgMethods invokes every method on v that takes no arguments and
// does not panic on a nil or populated receiver.
func callZeroArgMethods(t *testing.T, v reflect.Value, onlyGetters bool) {
	t.Helper()
	for i := 0; i < v.NumMethod(); i++ {
		m := v.Type().Method(i)
		if m.Type.NumIn() != 1 || m.Type.IsVariadic() {
			continue
		}
		if onlyGetters && !strings.HasPrefix(m.Name, "Get") {
			continue
		}
		v.Method(i).Call(nil)
	}
}

// populate sets every field of m to a non-default value, recursing into
// message fields one level so nested getters see data.
func populate(m protoreflect.Message, depth int) {
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		switch {
		case fd.IsList():
			list := m.Mutable(fd).List()
			list.Append(sampleValue(fd, list.NewElement, depth))
		case fd.IsMap():
			mp := m.Mutable(fd).Map()
			mp.Set(protoreflect.ValueOfString("k").MapKey(), sampleValue(fd.MapValue(), mp.NewValue, depth))
		default:
			m.Set(fd, sampleValue(fd, func() protoreflect.Value { return m.NewField(fd) }, depth))
		}
	}
}

func sampleValue(fd protoreflect.FieldDescriptor, newVal func() protoreflect.Value, depth int) protoreflect.Value {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true)
	case protoreflect.StringKind:
		return protoreflect.ValueOfString("s")
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes([]byte("b"))
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(7)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(7)
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32(7)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64(7)
	case protoreflect.FloatKind:
		return protoreflect.ValueOfFloat32(1.5)
	case protoreflect.DoubleKind:
		return protoreflect.ValueOfFloat64(1.5)
	case protoreflect.EnumKind:
		vals := fd.Enum().Values()
		return protoreflect.ValueOfEnum(vals.Get(vals.Len() - 1).Number())
	case protoreflect.MessageKind, protoreflect.GroupKind:
		v := newVal()
		if depth > 0 {
			populate(v.Message(), depth-1)
		}
		return v
	}
	return newVal()
}

func TestEveryMessageAccessorAndRoundTrip(t *testing.T) {
	seen := 0
	for _, file := range allFiles() {
		msgs := file.Messages()
		for i := 0; i < msgs.Len(); i++ {
			md := msgs.Get(i)
			mt, err := protoregistry.GlobalTypes.FindMessageByName(md.FullName())
			if err != nil {
				t.Fatalf("%s not registered: %v", md.FullName(), err)
			}
			seen++
			// Nil receiver: every getter must return the zero value.
			nilMsg := reflect.New(reflect.TypeOf(mt.Zero().Interface()).Elem())
			callZeroArgMethods(t, reflect.Zero(nilMsg.Type()), true)

			// Populated receiver, then a marshal/unmarshal round trip.
			full := mt.New()
			populate(full, 2)
			goVal := reflect.ValueOf(full.Interface())
			callZeroArgMethods(t, goVal, false)
			raw, err := proto.Marshal(full.Interface())
			if err != nil {
				t.Fatalf("%s marshal: %v", md.FullName(), err)
			}
			back := mt.New().Interface()
			if err := proto.Unmarshal(raw, back); err != nil {
				t.Fatalf("%s unmarshal: %v", md.FullName(), err)
			}
			if !proto.Equal(full.Interface(), back) {
				t.Errorf("%s round trip lost data", md.FullName())
			}
			// Reset then String on the emptied message.
			proto.Reset(back)
			callZeroArgMethods(t, reflect.ValueOf(back), false)
		}
		enums := file.Enums()
		for i := 0; i < enums.Len(); i++ {
			ed := enums.Get(i)
			et, err := protoregistry.GlobalTypes.FindEnumByName(ed.FullName())
			if err != nil {
				t.Fatalf("%s not registered: %v", ed.FullName(), err)
			}
			for j := 0; j < ed.Values().Len(); j++ {
				e := et.New(ed.Values().Get(j).Number())
				v := reflect.ValueOf(e)
				callZeroArgMethods(t, v, false)
				p := reflect.New(v.Type())
				p.Elem().Set(v)
				callZeroArgMethods(t, p, false)
				if s := v.MethodByName("String").Call(nil)[0].String(); s == "" {
					t.Errorf("%s value %d has empty String()", ed.FullName(), j)
				}
			}
		}
	}
	if seen < 40 {
		t.Errorf("only %d messages found; the subset shrank unexpectedly", seen)
	}
}

func TestFieldNumbersMatchUpstream(t *testing.T) {
	// A few load-bearing field numbers from Substrate's ateapi/v1 protos;
	// renumbering any of these would silently talk past the server.
	checks := map[string]map[string]int32{
		"substrate.ateapi.v1.ObjectRef":        {"atespace": 1, "name": 2},
		"substrate.ateapi.v1.ResourceMetadata": {"atespace": 1, "name": 2},
		"substrate.ateapi.v1.Actor":            {"metadata": 1, "actor_template": 2, "worker_selector": 3, "status": 5},
		"substrate.ateapi.v1.Container":        {"name": 1, "image": 2, "command": 3, "env": 5, "volume_mounts": 7},
	}
	for name, fields := range checks {
		md, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		m := md.(protoreflect.MessageDescriptor)
		for fname, want := range fields {
			fd := m.Fields().ByName(protoreflect.Name(fname))
			if fd == nil {
				t.Errorf("%s.%s missing", name, fname)
				continue
			}
			if int32(fd.Number()) != want {
				t.Errorf("%s.%s = %d, want %d", name, fname, fd.Number(), want)
			}
		}
	}
}

// fakeServer answers every RPC by echoing the request's target back.
type fakeServer struct {
	UnimplementedControlServer
	calls []string
}

func (f *fakeServer) note(name string) { f.calls = append(f.calls, name) }

func (f *fakeServer) GetActor(_ context.Context, r *GetActorRequest) (*Actor, error) {
	f.note("GetActor")
	return &Actor{Metadata: &ResourceMetadata{Name: r.GetActor().GetName()}, Status: &ActorStatus{State: ActorState_ACTOR_STATE_RUNNING}}, nil
}
func (f *fakeServer) CreateActor(_ context.Context, r *CreateActorRequest) (*Actor, error) {
	f.note("CreateActor")
	return r.GetActor(), nil
}
func (f *fakeServer) SuspendActor(_ context.Context, r *SuspendActorRequest) (*SuspendActorResponse, error) {
	f.note("SuspendActor:" + r.GetOptions().GetMode().String())
	return &SuspendActorResponse{}, nil
}
func (f *fakeServer) ResumeActor(context.Context, *ResumeActorRequest) (*ResumeActorResponse, error) {
	f.note("ResumeActor")
	return &ResumeActorResponse{}, nil
}
func (f *fakeServer) DeleteActor(_ context.Context, r *DeleteActorRequest) (*Actor, error) {
	f.note("DeleteActor")
	return &Actor{Metadata: &ResourceMetadata{Name: r.GetActor().GetName()}}, nil
}
func (f *fakeServer) CreateActorEgressPolicy(_ context.Context, r *CreateActorEgressPolicyRequest) (*EgressPolicy, error) {
	f.note("CreateActorEgressPolicy")
	return r.GetEgressPolicy(), nil
}
func (f *fakeServer) ListWorkers(_ context.Context, r *ListWorkersRequest) (*ListWorkersResponse, error) {
	f.note("ListWorkers:" + r.GetOptions().GetPageToken())
	if r.GetOptions().GetPageToken() == "" {
		return &ListWorkersResponse{Workers: []*Worker{{Metadata: &ResourceMetadata{Name: "w1"}}}, NextPageToken: "p2"}, nil
	}
	return &ListWorkersResponse{Workers: []*Worker{{Metadata: &ResourceMetadata{Name: "w2"}}}}, nil
}
func (f *fakeServer) CreateActorTemplate(_ context.Context, r *CreateActorTemplateRequest) (*ActorTemplate, error) {
	f.note("CreateActorTemplate")
	return r.GetActorTemplate(), nil
}
func (f *fakeServer) GetActorTemplate(_ context.Context, r *GetActorTemplateRequest) (*ActorTemplate, error) {
	f.note("GetActorTemplate")
	return &ActorTemplate{Metadata: &ResourceMetadata{Name: r.GetActorTemplate().GetName()}}, nil
}
func (f *fakeServer) DeleteActorTemplate(_ context.Context, r *DeleteActorTemplateRequest) (*ActorTemplate, error) {
	f.note("DeleteActorTemplate")
	return &ActorTemplate{Metadata: &ResourceMetadata{Name: r.GetActorTemplate().GetName()}}, nil
}

func dialBufconn(t *testing.T, srv ControlServer) ControlClient {
	t.Helper()
	ln := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	RegisterControlServer(gs, srv)
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return ln.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return NewControlClient(conn)
}

func TestControlClientRoundTripsEveryRPC(t *testing.T) {
	srv := &fakeServer{}
	c := dialBufconn(t, srv)
	ctx := context.Background()
	ref := &ObjectRef{Atespace: "hive", Name: "a1"}

	tpl, err := c.CreateActorTemplate(ctx, &CreateActorTemplateRequest{ActorTemplate: &ActorTemplate{Metadata: &ResourceMetadata{Name: "t1"}}})
	if err != nil || tpl.GetMetadata().GetName() != "t1" {
		t.Fatalf("CreateActorTemplate: %v %v", tpl, err)
	}
	if got, err := c.GetActorTemplate(ctx, &GetActorTemplateRequest{ActorTemplate: ref}); err != nil || got.GetMetadata().GetName() != "a1" {
		t.Fatalf("GetActorTemplate: %v %v", got, err)
	}
	if got, err := c.DeleteActorTemplate(ctx, &DeleteActorTemplateRequest{ActorTemplate: ref}); err != nil || got.GetMetadata().GetName() != "a1" {
		t.Fatalf("DeleteActorTemplate: %v %v", got, err)
	}
	actor, err := c.CreateActor(ctx, &CreateActorRequest{Actor: &Actor{Metadata: &ResourceMetadata{Name: "a1"}, ActorTemplate: ref}})
	if err != nil || actor.GetActorTemplate().GetName() != "a1" {
		t.Fatalf("CreateActor: %v %v", actor, err)
	}
	if got, err := c.GetActor(ctx, &GetActorRequest{Actor: ref}); err != nil || got.GetStatus().GetState() != ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("GetActor: %v %v", got, err)
	}
	if _, err := c.ResumeActor(ctx, &ResumeActorRequest{Actor: ref}); err != nil {
		t.Fatalf("ResumeActor: %v", err)
	}
	if _, err := c.SuspendActor(ctx, &SuspendActorRequest{Actor: ref, Options: &SuspendOptions{Mode: DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS}}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	pol, err := c.CreateActorEgressPolicy(ctx, &CreateActorEgressPolicyRequest{Actor: ref, EgressPolicy: &EgressPolicy{Metadata: &ResourceMetadata{Name: "default"}}})
	if err != nil || pol.GetMetadata().GetName() != "default" {
		t.Fatalf("CreateActorEgressPolicy: %v %v", pol, err)
	}
	if got, err := c.DeleteActor(ctx, &DeleteActorRequest{Actor: ref}); err != nil || got.GetMetadata().GetName() != "a1" {
		t.Fatalf("DeleteActor: %v %v", got, err)
	}
	page1, err := c.ListWorkers(ctx, &ListWorkersRequest{Options: &ListOptions{}})
	if err != nil || page1.GetNextPageToken() != "p2" || len(page1.GetWorkers()) != 1 {
		t.Fatalf("ListWorkers page 1: %v %v", page1, err)
	}
	page2, err := c.ListWorkers(ctx, &ListWorkersRequest{Options: &ListOptions{PageToken: "p2"}})
	if err != nil || page2.GetNextPageToken() != "" || page2.GetWorkers()[0].GetMetadata().GetName() != "w2" {
		t.Fatalf("ListWorkers page 2: %v %v", page2, err)
	}
	joined := strings.Join(srv.calls, ",")
	for _, want := range []string{"CreateActorTemplate", "GetActorTemplate", "DeleteActorTemplate", "CreateActor", "GetActor", "ResumeActor", "SuspendActor:DURABILITY_MODE_ASYNCHRONOUS", "CreateActorEgressPolicy", "DeleteActor", "ListWorkers:", "ListWorkers:p2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("server never saw %s: %s", want, joined)
		}
	}
}

func TestUnimplementedServerAnswersUnimplemented(t *testing.T) {
	c := dialBufconn(t, UnimplementedControlServer{})
	ctx := context.Background()
	calls := []func() error{
		func() error { _, err := c.GetActor(ctx, &GetActorRequest{}); return err },
		func() error { _, err := c.CreateActor(ctx, &CreateActorRequest{}); return err },
		func() error { _, err := c.SuspendActor(ctx, &SuspendActorRequest{}); return err },
		func() error { _, err := c.ResumeActor(ctx, &ResumeActorRequest{}); return err },
		func() error { _, err := c.DeleteActor(ctx, &DeleteActorRequest{}); return err },
		func() error { _, err := c.CreateActorEgressPolicy(ctx, &CreateActorEgressPolicyRequest{}); return err },
		func() error { _, err := c.ListWorkers(ctx, &ListWorkersRequest{}); return err },
		func() error { _, err := c.CreateActorTemplate(ctx, &CreateActorTemplateRequest{}); return err },
		func() error { _, err := c.GetActorTemplate(ctx, &GetActorTemplateRequest{}); return err },
		func() error { _, err := c.DeleteActorTemplate(ctx, &DeleteActorTemplateRequest{}); return err },
	}
	for i, call := range calls {
		err := call()
		if st, ok := status.FromError(err); !ok || st.Code() != codes.Unimplemented {
			t.Errorf("call %d: %v, want Unimplemented", i, err)
		}
	}
	var u UnimplementedControlServer
	u.testEmbeddedByValue()
	if _, err := u.GetActor(ctx, nil); err == nil {
		t.Error("direct GetActor on the unimplemented server returned nil error")
	}
	var _ UnsafeControlServer = &fakeServer{}
}

func TestRegisterControlServerRejectsNilEmbedding(t *testing.T) {
	// Registering a server whose embedded UnimplementedControlServer is a
	// nil pointer must panic, which is grpc-go's guard against a forgotten
	// embedding.
	defer func() {
		if r := recover(); r == nil {
			t.Error("no panic for a nil embedded UnimplementedControlServer")
		}
	}()
	type nilEmbed struct{ *UnimplementedControlServer }
	RegisterControlServer(grpc.NewServer(), nilEmbed{})
}

func TestServiceDescriptor(t *testing.T) {
	if Control_ServiceDesc.ServiceName != "substrate.ateapi.v1.Control" {
		t.Errorf("service name = %q", Control_ServiceDesc.ServiceName)
	}
	if len(Control_ServiceDesc.Methods) != 10 {
		t.Errorf("methods = %d, want 10", len(Control_ServiceDesc.Methods))
	}
	if !errors.Is(context.Canceled, context.Canceled) {
		t.Fatal("unreachable")
	}
}
