package dahook

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

const interruptType = "hook_invocation"

// Capability is a shared per-session authentication capability. It is safe to
// format, but must be delivered to the client over an authenticated channel.
type Capability struct{ key [32]byte }

// NewCapability constructs a capability from at least 32 bytes of
// cryptographically random caller-owned key material.
func NewCapability(key []byte) Capability {
	if len(key) < 32 {
		panic("dahook: capability key must contain at least 32 bytes")
	}
	return Capability{key: sha256.Sum256(key)}
}

func (Capability) String() string   { return "dahook.Capability(<redacted>)" }
func (Capability) GoString() string { return "dahook.Capability(<redacted>)" }

func (capability Capability) valid() bool {
	return capability.key != [32]byte{}
}

// newRequest creates and authenticates one bounded server-owned request.
func newRequest(capability Capability, snapshotID, runID string, invocation Invocation, deadline time.Time) InvocationRequest {
	if snapshotID == "" || runID == "" || invocation.validate() != nil || EventOwner(invocation.Event) != ServerOwner {
		panic("dahook: invalid server hook request")
	}
	if !capability.valid() {
		panic("dahook: interrupt capability is required")
	}
	if deadline.IsZero() {
		deadline = time.Now().Add(defaultTimeout)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(fmt.Sprintf("dahook: random invocation id: %v", err))
	}
	request := InvocationRequest{ProtocolVersion: 1, InvocationID: hex.EncodeToString(id[:]), SnapshotID: snapshotID, RunID: runID, Invocation: freezeInvocation(invocation), Deadline: deadline.UTC()}
	request.CapabilityMAC = capability.sign(request)
	return request
}

// BuildInterrupt creates the stable graph payload.
func BuildInterrupt(request InvocationRequest) Interrupt {
	return Interrupt{Type: interruptType, Request: request}
}

// Fulfiller authenticates and replay-protects server requests before allowing
// the client Engine to run any hook side effects.
type Fulfiller struct {
	engine     *Engine
	capability Capability
	ledger     *clientLedger
}

// NewFulfiller constructs a client-side interrupt fulfiller. Zero capacity
// selects a bounded 1024-request session ledger.
func NewFulfiller(engine *Engine, capability Capability, capacity int) *Fulfiller {
	if engine == nil || !capability.valid() {
		panic("dahook: engine and interrupt capability are required")
	}
	if capacity < 0 {
		panic("dahook: negative client ledger capacity")
	}
	if capacity == 0 {
		capacity = 1024
	}
	return &Fulfiller{engine: engine, capability: capability, ledger: &clientLedger{capacity: capacity, states: map[string]clientInvocationState{}}}
}

// Fulfill authenticates an immutable copy, claims its invocation id
// atomically, and only then runs the requested hook.
func (fulfiller *Fulfiller) Fulfill(ctx context.Context, request InvocationRequest) (InvocationResponse, error) {
	request, err := fulfiller.capability.authenticate(request)
	if err != nil || request.SnapshotID != fulfiller.engine.snapshot.ID {
		return InvocationResponse{}, fmt.Errorf("dahook: invalid or stale invocation request")
	}
	if time.Now().After(request.Deadline) {
		return InvocationResponse{}, context.DeadlineExceeded
	}
	if err := fulfiller.ledger.claim(request.InvocationID); err != nil {
		return InvocationResponse{}, err
	}
	defer fulfiller.ledger.consume(request.InvocationID)
	run, cancel := context.WithDeadline(ctx, request.Deadline)
	defer cancel()
	decision, err := fulfiller.engine.Run(run, request.Invocation)
	if err != nil {
		return InvocationResponse{}, err
	}
	response := InvocationResponse{ProtocolVersion: 1, InvocationID: request.InvocationID, SnapshotID: request.SnapshotID, Decision: decision}
	response.CapabilityMAC = fulfiller.capability.signResponse(request, response)
	return response, nil
}

type clientInvocationState uint8

const (
	clientInvocationInProgress clientInvocationState = iota + 1
	clientInvocationConsumed
)

type clientLedger struct {
	mu            sync.Mutex
	capacity      int
	states        map[string]clientInvocationState
	consumedOrder []string
}

func (ledger *clientLedger) claim(id string) error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if state := ledger.states[id]; state != 0 {
		if state == clientInvocationInProgress {
			return fmt.Errorf("dahook: hook invocation is already in progress")
		}
		return fmt.Errorf("dahook: hook invocation was already consumed")
	}
	if len(ledger.states) >= ledger.capacity {
		ledger.trimConsumed()
	}
	if len(ledger.states) >= ledger.capacity {
		return fmt.Errorf("dahook: in-progress invocation capacity exceeded")
	}
	ledger.states[id] = clientInvocationInProgress
	return nil
}

func (ledger *clientLedger) consume(id string) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.states[id] == clientInvocationInProgress {
		ledger.states[id] = clientInvocationConsumed
		ledger.consumedOrder = append(ledger.consumedOrder, id)
	}
}

func (ledger *clientLedger) trimConsumed() {
	for len(ledger.states) >= ledger.capacity && len(ledger.consumedOrder) > 0 {
		id := ledger.consumedOrder[0]
		ledger.consumedOrder = ledger.consumedOrder[1:]
		if ledger.states[id] == clientInvocationConsumed {
			delete(ledger.states, id)
		}
	}
}

// Ledger rejects mismatched and duplicate resume fulfillment. It is safe for
// concurrent graph calls and has a bounded useful default capacity.
type Ledger struct {
	mu        sync.Mutex
	capacity  int
	pending   map[string]InvocationRequest
	fulfilled map[string]InvocationResponse
	order     []string
}

// Server owns creation and validation of graph interrupt requests. It never
// executes hook commands; a client Engine fulfills the emitted payload.
type Server struct {
	snapshotID string
	ledger     *Ledger
	capability Capability
}

// NewServer constructs a server-side interrupt dispatcher. The ledger is a
// required positional dependency because replay protection is not optional.
func NewServer(snapshotID string, ledger *Ledger, capability Capability) *Server {
	if snapshotID == "" || ledger == nil || !capability.valid() {
		panic("dahook: snapshot id, fulfillment ledger, and interrupt capability are required")
	}
	return &Server{snapshotID: snapshotID, ledger: ledger, capability: capability}
}

// Interrupt registers and returns one server-owned lifecycle request.
func (server *Server) Interrupt(runID string, invocation Invocation, deadline time.Time) (Interrupt, error) {
	request := newRequest(server.capability, server.snapshotID, runID, invocation, deadline)
	if err := server.ledger.Register(request); err != nil {
		return Interrupt{}, err
	}
	return BuildInterrupt(request), nil
}

type unsignedInvocationRequest struct {
	ProtocolVersion int        `json:"protocol_version"`
	InvocationID    string     `json:"invocation_id"`
	SnapshotID      string     `json:"snapshot_id"`
	RunID           string     `json:"run_id"`
	Invocation      Invocation `json:"invocation"`
	Deadline        time.Time  `json:"deadline"`
}

func unsignedRequest(request InvocationRequest) unsignedInvocationRequest {
	return unsignedInvocationRequest{
		ProtocolVersion: request.ProtocolVersion,
		InvocationID:    request.InvocationID,
		SnapshotID:      request.SnapshotID,
		RunID:           request.RunID,
		Invocation:      request.Invocation,
		Deadline:        request.Deadline,
	}
}

func (capability Capability) sign(request InvocationRequest) string {
	raw, err := json.Marshal(unsignedRequest(request))
	if err != nil {
		panic(fmt.Sprintf("dahook: encode invocation capability: %v", err))
	}
	mac := hmac.New(sha256.New, capability.key[:])
	_, _ = mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

func (capability Capability) authenticate(input InvocationRequest) (InvocationRequest, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return InvocationRequest{}, err
	}
	var request InvocationRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return InvocationRequest{}, err
	}
	_, validEvent := allEvents[request.Invocation.Event]
	if request.ProtocolVersion != 1 || request.InvocationID == "" || request.SnapshotID == "" || request.RunID == "" || request.Deadline.IsZero() || request.CapabilityMAC == "" || !validEvent || EventOwner(request.Invocation.Event) != ServerOwner || request.Invocation.validate() != nil {
		return InvocationRequest{}, fmt.Errorf("dahook: malformed invocation capability")
	}
	provided, err := hex.DecodeString(request.CapabilityMAC)
	if err != nil || len(provided) != sha256.Size {
		return InvocationRequest{}, fmt.Errorf("dahook: malformed invocation capability")
	}
	expected, err := json.Marshal(unsignedRequest(request))
	if err != nil {
		return InvocationRequest{}, err
	}
	mac := hmac.New(sha256.New, capability.key[:])
	_, _ = mac.Write(expected)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return InvocationRequest{}, fmt.Errorf("dahook: unauthenticated invocation request")
	}
	return request, nil
}

func freezeInvocation(invocation Invocation) Invocation {
	raw, err := json.Marshal(invocation)
	if err != nil {
		panic(fmt.Sprintf("dahook: encode hook invocation: %v", err))
	}
	var frozen Invocation
	if err := json.Unmarshal(raw, &frozen); err != nil {
		panic(fmt.Sprintf("dahook: decode hook invocation: %v", err))
	}
	return frozen
}

// Resume consumes an exact client response and returns its normalized decision.
func (server *Server) Resume(response InvocationResponse) (Decision, error) {
	return server.ledger.accept(response, server.capability)
}

// NewLedger constructs a replay ledger. Zero capacity selects 1024 entries.
func NewLedger(capacity int) *Ledger {
	if capacity < 0 {
		panic("dahook: negative ledger capacity")
	}
	if capacity == 0 {
		capacity = 1024
	}
	return &Ledger{capacity: capacity, pending: map[string]InvocationRequest{}, fulfilled: map[string]InvocationResponse{}}
}

// Register records an outstanding request idempotently and rejects collisions.
func (ledger *Ledger) Register(request InvocationRequest) error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	_, validEvent := allEvents[request.Invocation.Event]
	if request.ProtocolVersion != 1 || request.InvocationID == "" || request.SnapshotID == "" || request.RunID == "" || request.Deadline.IsZero() || request.CapabilityMAC == "" || !validEvent || EventOwner(request.Invocation.Event) != ServerOwner {
		return fmt.Errorf("dahook: invalid invocation request")
	}
	if prior, ok := ledger.pending[request.InvocationID]; ok {
		if prior.SnapshotID != request.SnapshotID || prior.RunID != request.RunID || prior.CapabilityMAC != request.CapabilityMAC {
			return fmt.Errorf("dahook: invocation id collision")
		}
		return nil
	}
	if len(ledger.pending) >= ledger.capacity {
		return fmt.Errorf("dahook: pending invocation capacity exceeded")
	}
	ledger.pending[request.InvocationID] = request
	ledger.order = append(ledger.order, request.InvocationID)
	ledger.trim()
	return nil
}

// accept authenticates a response against its exact request before consuming
// that pending request, then rejects replay.
func (ledger *Ledger) accept(response InvocationResponse, capability Capability) (Decision, error) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if _, exists := ledger.fulfilled[response.InvocationID]; exists {
		return Decision{}, fmt.Errorf("dahook: duplicate hook fulfillment")
	}
	request, ok := ledger.pending[response.InvocationID]
	if !ok || response.ProtocolVersion != 1 || response.SnapshotID != request.SnapshotID {
		return Decision{}, fmt.Errorf("dahook: mismatched hook fulfillment")
	}
	if err := capability.authenticateResponse(request, response); err != nil {
		return Decision{}, err
	}
	delete(ledger.pending, response.InvocationID)
	ledger.fulfilled[response.InvocationID] = response
	return response.Decision, nil
}

type authenticatedInvocationResponse struct {
	ProtocolVersion      int      `json:"protocol_version"`
	InvocationID         string   `json:"invocation_id"`
	SnapshotID           string   `json:"snapshot_id"`
	RunID                string   `json:"run_id"`
	RequestCapabilityMAC string   `json:"request_capability_mac"`
	Decision             Decision `json:"decision"`
}

func responseAuthenticationPayload(request InvocationRequest, response InvocationResponse) authenticatedInvocationResponse {
	return authenticatedInvocationResponse{
		ProtocolVersion: response.ProtocolVersion, InvocationID: response.InvocationID,
		SnapshotID: response.SnapshotID, RunID: request.RunID,
		RequestCapabilityMAC: request.CapabilityMAC, Decision: response.Decision,
	}
}

func (capability Capability) signResponse(request InvocationRequest, response InvocationResponse) string {
	raw, err := json.Marshal(responseAuthenticationPayload(request, response))
	if err != nil {
		panic(fmt.Sprintf("dahook: encode response capability: %v", err))
	}
	mac := hmac.New(sha256.New, capability.key[:])
	_, _ = mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

func (capability Capability) authenticateResponse(request InvocationRequest, response InvocationResponse) error {
	provided, err := hex.DecodeString(response.CapabilityMAC)
	if err != nil || len(provided) != sha256.Size {
		return fmt.Errorf("dahook: malformed response capability")
	}
	expected := capability.signResponse(request, response)
	expectedBytes, _ := hex.DecodeString(expected)
	if !hmac.Equal(provided, expectedBytes) {
		return fmt.Errorf("dahook: unauthenticated hook fulfillment")
	}
	return nil
}
func (ledger *Ledger) trim() {
	for len(ledger.order) > ledger.capacity {
		removed := false
		for index, id := range ledger.order {
			if _, pending := ledger.pending[id]; pending {
				continue
			}
			ledger.order = append(ledger.order[:index], ledger.order[index+1:]...)
			delete(ledger.fulfilled, id)
			removed = true
			break
		}
		if !removed {
			return
		}
	}
}
