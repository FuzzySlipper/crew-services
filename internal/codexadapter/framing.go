package codexadapter

import (
	"bytes"
	"encoding/json"
	"strings"
)

// frameInbound makes fabric provenance explicit without exposing native IDs.
// Replies are terminal by default: agents may use their own new ordinary
// message, but should not reflexively answer the linked reply again.
func frameInbound(message Message) string {
	header := inboundFrameHeader{
		Type:           "crew_delivery",
		MessageID:      message.MessageID,
		From:           message.SenderAddress,
		To:             message.RecipientAddress,
		Kind:           "ordinary",
		ReplyToMessage: "",
	}
	instruction := "If a response is warranted, send a linked reply using crew_message(recipient=" + frameJSON(message.SenderAddress) + ", reply_to_message_id=" + frameJSON(message.MessageID) + ", text=\"...\")."
	if message.ReplyToMessageID != "" {
		header.Kind = "reply"
		header.ReplyToMessage = message.ReplyToMessageID
		instruction = "This is a reply acknowledging prior message " + frameJSON(message.ReplyToMessageID) + ". Do not reply merely because this message is a reply. Only if its body independently requires further work, send a new ordinary crew_message without reply_to_message_id."
	}
	return frameJSON(header) + "\n" + instruction + "\n<crew-message-body encoding=\"json\">\n" + frameJSON(message.Body) + "\n</crew-message-body>"
}

type inboundFrameHeader struct {
	Type           string `json:"type"`
	MessageID      string `json:"message_id"`
	From           string `json:"from"`
	To             string `json:"to"`
	Kind           string `json:"kind"`
	ReplyToMessage string `json:"reply_to_message_id,omitempty"`
}

// frameJSON mirrors JSON.stringify's compact, non-HTML-escaped output. The
// encoded body remains one JSON string even when its contents resemble frame
// delimiters or metadata.
func frameJSON(value any) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "null"
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}
