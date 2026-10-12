package ingestion

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/types/events"
)

// This private DTO intentionally has no protobuf, text, caption, media, error,
// token or session-state field. Exact identifiers are never public diagnostics.
type mutationWitness struct {
	Branch                                           string `json:"branch"`
	SDK                                              string `json:"sdk"`
	Observation                                      string `json:"observation"`
	EnvelopePresent                                  bool   `json:"envelopePresent"`
	SourceWebPresent                                 bool   `json:"sourceWebPresent"`
	ProtocolPresent                                  bool   `json:"protocolPresent"`
	ProtocolType                                     int32  `json:"protocolType"`
	KeyPresent                                       bool   `json:"keyPresent"`
	Chat, Sender, Message                            string
	TargetChat, TargetSender, TargetMessage          string
	TimestampMillis, RevisionMillis                  int64
	FromMe, DirectionVerified, TargetMappingVerified bool
}

func witnessIdentifier(s string) string {
	if len(s) > 256 || !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
		return ""
	}
	return s
}
func (r *Runtime) mutationMetadataMissing(branch string, event *events.Message) {
	// A failed diagnostic write never delays or suppresses the safety fence.
	r.writeMutationWitness(branch, event)
	r.failClosed("mutation_metadata_missing")
}
func (r *Runtime) writeMutationWitness(branch string, event *events.Message) {
	switch branch {
	case "edit_protocol_missing", "group_admin_control", "expiry_overflow", "expiry_missing", "delete_for_me_author_missing":
	default:
		return
	}
	if r.config.DiagnosticDir == "" {
		return
	}
	w := mutationWitness{Branch: branch, SDK: "6bb48c0f1ff0", EnvelopePresent: event != nil}
	r.mu.Lock()
	w.Observation = witnessIdentifier(string(r.observation))
	r.mu.Unlock()
	if event != nil {
		w.SourceWebPresent = event.SourceWebMsg != nil
		w.Chat = witnessIdentifier(event.Info.Chat.ToNonAD().String())
		w.Sender = witnessIdentifier(event.Info.Sender.ToNonAD().String())
		w.Message = witnessIdentifier(event.Info.ID)
		w.TimestampMillis = event.Info.Timestamp.UnixMilli()
		w.FromMe = event.Info.IsFromMe
		// Envelope receipt is authenticated, but does not prove an original target.
		w.DirectionVerified = false
		w.TargetMappingVerified = false
		if p := mutationProtocol(event); p != nil {
			w.ProtocolPresent = true
			w.ProtocolType = int32(p.GetType())
			w.RevisionMillis = p.GetTimestampMS()
			w.KeyPresent = p.Key != nil
			w.TargetChat = witnessIdentifier(p.GetKey().GetRemoteJID())
			w.TargetSender = witnessIdentifier(p.GetKey().GetParticipant())
			w.TargetMessage = witnessIdentifier(p.GetKey().GetID())
		}
	}
	encoded, err := json.Marshal(w)
	if err != nil || len(encoded) > 4096 {
		return
	}
	r.diagnosticMu.Lock()
	defer r.diagnosticMu.Unlock()
	if privateDirectory(r.config.DiagnosticDir, false) != nil {
		return
	}
	root, err := os.OpenRoot(r.config.DiagnosticDir)
	if err != nil {
		return
	}
	defer root.Close()
	// Finite branch slots cap disk retention at 20KiB. Atomic replacement avoids
	// partial JSON; directory sync makes the witness precede the safety marker.
	name := fmt.Sprintf("mutation-%s.json", branch)
	tmp := name + ".pending"
	// Retire only this fixed private staging name after an interrupted write.
	_ = root.Remove(tmp)
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer root.Remove(tmp)
	_, err = f.Write(encoded)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return
	}
	if root.Rename(tmp, name) != nil {
		return
	}
	dir, err := root.Open(".")
	if err != nil {
		return
	}
	_ = dir.Sync()
	_ = dir.Close()
}
