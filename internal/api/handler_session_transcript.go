package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

type sessionTranscriptResponse struct {
	ID         string                       `json:"id"`
	Template   string                       `json:"template"`
	Format     string                       `json:"format"`
	Turns      []outputTurn                 `json:"turns"`
	Pagination *worker.TranscriptPagination `json:"pagination,omitempty"`
}

type sessionRawTranscriptResponse struct {
	ID         string                       `json:"id"`
	Template   string                       `json:"template"`
	Format     string                       `json:"format"`
	Messages   []SessionRawMessageFrame     `json:"messages"`
	Pagination *worker.TranscriptPagination `json:"pagination,omitempty"`
}

func (s *Server) handleSessionTranscript(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := s.resolveSessionIDAllowClosedWithConfig(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	catalog, err := s.workerSessionCatalog(store)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	info, err := catalog.Get(id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	path, err := handle.TranscriptPath(r.Context())
	if err != nil && !errors.Is(err, worker.ErrHistoryUnavailable) {
		writeSessionManagerError(w, err)
		return
	}

	format := r.URL.Query().Get("format")
	wantRaw := format == "raw"
	wantStructured := format == "structured"

	if path != "" {
		tail := 0
		if v := r.URL.Query().Get("tail"); v != "" {
			if n, convErr := strconv.Atoi(v); convErr == nil && n >= 0 {
				tail = n
			}
		}
		before := r.URL.Query().Get("before")
		after := r.URL.Query().Get("after")

		if before != "" && after != "" {
			writeError(w, http.StatusUnprocessableEntity, "invalid_params", "before and after are mutually exclusive")
			return
		}

		if wantStructured {
			history, historyErr := handle.History(worker.WithoutOperationEvents(r.Context()), worker.HistoryRequest{
				TailCompactions: tail,
				BeforeEntryID:   before,
				AfterEntryID:    after,
			})
			if historyErr != nil {
				if errors.Is(historyErr, worker.ErrHistoryUnavailable) {
					writeJSON(w, http.StatusOK, legacyStructuredFallbackTranscriptResponse(r.Context(), info, handle))
					return
				}
				writeError(w, http.StatusInternalServerError, "internal", "reading session history: "+historyErr.Error())
				return
			}
			messages, _ := historySnapshotStructuredMessages(history, queryBoolParam(r, "include_thinking"))
			writeJSON(w, http.StatusOK, sessionTranscriptGetResponse{
				ID:                 info.ID,
				Template:           info.Template,
				Provider:           info.Provider,
				Format:             "structured",
				SchemaVersion:      sessionStructuredSchemaVersion,
				History:            structuredHistoryFromSnapshot(history),
				StructuredMessages: messages,
				Pagination:         history.Pagination,
			})
			return
		}

		if wantRaw {
			transcript, err := handle.Transcript(r.Context(), worker.TranscriptRequest{
				TailCompactions: tail,
				BeforeEntryID:   before,
				AfterEntryID:    after,
				Raw:             true,
			})
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "reading session log: "+err.Error())
				return
			}
			writeJSON(w, http.StatusOK, sessionRawTranscriptResponse{
				ID:         info.ID,
				Template:   info.Template,
				Format:     "raw",
				Messages:   wrapRawFrameBytes(transcript.RawMessages),
				Pagination: transcript.Session.Pagination,
			})
			return
		}

		transcript, err := handle.Transcript(r.Context(), worker.TranscriptRequest{
			TailCompactions: tail,
			BeforeEntryID:   before,
			AfterEntryID:    after,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "reading session log: "+err.Error())
			return
		}
		sess := transcript.Session

		turns := make([]outputTurn, 0, len(sess.Messages))
		for _, entry := range sess.Messages {
			turn := entryToTurn(entry)
			if turn.Text == "" {
				continue
			}
			turns = append(turns, turn)
		}
		writeJSON(w, http.StatusOK, sessionTranscriptResponse{
			ID:         info.ID,
			Template:   info.Template,
			Format:     "conversation",
			Turns:      turns,
			Pagination: sess.Pagination,
		})
		return
	}

	if wantRaw {
		writeJSON(w, http.StatusOK, sessionRawTranscriptResponse{
			ID:       info.ID,
			Template: info.Template,
			Format:   "raw",
			Messages: []SessionRawMessageFrame{},
		})
		return
	}

	if wantStructured {
		writeJSON(w, http.StatusOK, legacyStructuredFallbackTranscriptResponse(r.Context(), info, handle))
		return
	}

	output, peekErr := handle.Peek(r.Context(), 100)
	if peekErr != nil && !errors.Is(peekErr, session.ErrSessionInactive) {
		writeError(w, http.StatusInternalServerError, "internal", peekErr.Error())
		return
	}
	if peekErr == nil {
		turns := []outputTurn{}
		if output != "" {
			turns = append(turns, outputTurn{Role: "output", Text: output})
		}
		writeJSON(w, http.StatusOK, sessionTranscriptResponse{
			ID:       info.ID,
			Template: info.Template,
			Format:   "text",
			Turns:    turns,
		})
		return
	}

	writeJSON(w, http.StatusOK, sessionTranscriptResponse{
		ID:       info.ID,
		Template: info.Template,
		Format:   "conversation",
		Turns:    []outputTurn{},
	})
}

func legacyStructuredFallbackTranscriptResponse(ctx context.Context, info session.Info, handle worker.PeekHandle) sessionTranscriptGetResponse {
	activity := string(worker.TailActivityIdle)
	output := ""
	peekOutput, peekErr := handle.Peek(ctx, 100)
	if peekErr == nil {
		activity = string(worker.TailActivityInTurn)
		output = peekOutput
	}
	return sessionTranscriptGetResponse{
		ID:                 info.ID,
		Template:           info.Template,
		Provider:           info.Provider,
		Format:             "structured",
		SchemaVersion:      sessionStructuredSchemaVersion,
		History:            structuredFallbackHistory(info.ID, info.SessionKey, activity),
		StructuredMessages: structuredFallbackMessages(info.ID, info.Provider, output),
	}
}

func queryBoolParam(r *http.Request, name string) bool {
	value := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name)))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}
