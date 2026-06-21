package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/spf13/cobra"
)

// envExternalOrigin is the env var the controller sets when nudging a session
// for an externally-originated message. Its value is a JSON-encoded
// externalOriginEnvelope carrying the ConversationRef the session should reply to.
const envExternalOrigin = "GC_EXTERNAL_ORIGIN"

// externalOriginEnvelope mirrors extmsg.ExternalOriginEnvelope for JSON
// decoding without importing internal/extmsg. Field names are PascalCase
// because the source struct has no json tags (Go default encoding).
type externalOriginEnvelope struct {
	Conversation genclient.ConversationRef `json:"Conversation"`
}

type extmsgReplyJSONResult struct {
	Delivered      bool   `json:"delivered"`
	ConversationID string `json:"conversation_id"`
	Sequence       int64  `json:"sequence"`
}

// extmsgReplyStdin is the stdin reader for --stdin. Indirected for tests.
var extmsgReplyStdin = func() io.Reader { return os.Stdin }

// extmsgReplyAPIClient is indirected for tests — they inject a client pointed
// at an httptest.Server or nil to force error paths.
var extmsgReplyAPIClient = func(cityPath string) *api.Client {
	return apiClient(cityPath)
}

// newExtmsgCmd returns the "gc extmsg" parent command.
func newExtmsgCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "extmsg",
		Short: "External messaging operations",
	}
	cmd.AddCommand(newExtmsgReplyCmd(stdout, stderr))
	return cmd
}

// newExtmsgReplyCmd returns the "gc extmsg reply" subcommand.
func newExtmsgReplyCmd(stdout, stderr io.Writer) *cobra.Command {
	var refJSON string
	var fromStdin bool
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "reply [flags] [text]",
		Short: "Reply to an external client over the outbound SSE channel",
		Long: `Reply to an external client over the outbound SSE channel.

When no --ref is given, the ConversationRef is read from GC_EXTERNAL_ORIGIN,
which the controller sets when nudging a session for an externally-originated
message. If GC_EXTERNAL_ORIGIN is not set, the command exits non-zero.`,
		Example: `  gc extmsg reply "one-liner response"
  gc extmsg reply --stdin <<'EOF'
  multi-line response
  EOF
  gc extmsg reply --ref '{"provider":"llm-client","account_id":"c1","conversation_id":"cv1","kind":"direct"}' "hello"`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc extmsg reply: %v\n", err)
				return errExit
			}
			c := extmsgReplyAPIClient(cityPath)
			if c == nil {
				fmt.Fprintln(stderr, "gc extmsg reply: no API server available (is the controller running?)")
				return errExit
			}
			if runExtmsgReply(c, refJSON, fromStdin, args, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&refJSON, "ref", "", "Explicit ConversationRef JSON; overrides session context")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "Read reply text from stdin instead of text argument")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output delivery status as JSON")

	return cmd
}

// runExtmsgReply is the core logic for gc extmsg reply, extracted for
// testability. refJSON is "" to read from GC_EXTERNAL_ORIGIN.
func runExtmsgReply(c *api.Client, refJSON string, fromStdin bool, args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if fromStdin && len(args) > 0 {
		fmt.Fprintln(stderr, "gc extmsg reply: --stdin and positional text argument are mutually exclusive")
		return 1
	}

	var text string
	if fromStdin {
		data, err := io.ReadAll(extmsgReplyStdin())
		if err != nil {
			fmt.Fprintf(stderr, "gc extmsg reply: reading stdin: %v\n", err)
			return 1
		}
		text = string(data)
	} else if len(args) > 0 {
		text = args[0]
	}

	conv, err := resolveExtmsgConv(refJSON)
	if err != nil {
		fmt.Fprintf(stderr, "gc extmsg reply: %v\n", err)
		return 1
	}

	sessionID := os.Getenv("GC_SESSION_ID")
	if sessionID == "" {
		fmt.Fprintln(stderr, "gc extmsg reply: GC_SESSION_ID not set; command must be called from within a running session")
		return 1
	}

	result, err := c.ExtMsgOutbound(context.Background(), sessionID, conv, text)
	if err != nil {
		fmt.Fprintf(stderr, "gc extmsg reply: error: %v\n", err)
		return 1
	}

	if jsonOutput {
		out := extmsgReplyJSONResult{
			Delivered:      result.Delivered,
			ConversationID: result.ConversationID,
			Sequence:       result.Sequence,
		}
		if err := json.NewEncoder(stdout).Encode(out); err != nil {
			fmt.Fprintf(stderr, "gc extmsg reply: encoding JSON: %v\n", err)
			return 1
		}
		return 0
	}

	if result.Delivered {
		fmt.Fprintf(stdout, "delivered %s seq:%d\n", result.ConversationID, result.Sequence)
	} else {
		fmt.Fprintf(stdout, "no-subscriber %s (transcript appended; client not connected)\n", result.ConversationID)
	}
	return 0
}

// resolveExtmsgConv resolves the ConversationRef for gc extmsg reply:
// if refJSON is non-empty, unmarshal it directly; otherwise decode the
// ExternalOriginEnvelope from GC_EXTERNAL_ORIGIN.
func resolveExtmsgConv(refJSON string) (genclient.ConversationRef, error) {
	if refJSON != "" {
		var conv genclient.ConversationRef
		if err := json.Unmarshal([]byte(refJSON), &conv); err != nil {
			return genclient.ConversationRef{}, fmt.Errorf("--ref: invalid ConversationRef JSON: %w", err)
		}
		return conv, nil
	}
	raw := os.Getenv(envExternalOrigin)
	if raw == "" {
		return genclient.ConversationRef{}, fmt.Errorf(
			"no ConversationRef: --ref not given and %s is not set in the session context", envExternalOrigin,
		)
	}
	var env externalOriginEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return genclient.ConversationRef{}, fmt.Errorf("decoding %s: %w", envExternalOrigin, err)
	}
	return env.Conversation, nil
}
