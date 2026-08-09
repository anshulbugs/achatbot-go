package llm_processors

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"
	"github.com/weedge/pipeline-go/pkg/frames"
	"github.com/weedge/pipeline-go/pkg/logger"
	"github.com/weedge/pipeline-go/pkg/processors"

	"achatbot/pkg/common"
	"achatbot/pkg/modules/functions"
	"achatbot/pkg/types"
	achatbot_frames "achatbot/pkg/types/frames"
)

type LLMOpenAIApiProcessor struct {
	*processors.AsyncFrameProcessor
	provider common.IOpenAILLMProvider
	session  *common.Session
	// turnMu guards turnCancel, which stops the in-flight generation when the
	// caller interrupts. Written from the pipeline goroutine delivering frames
	// and read from the goroutine running the turn.
	turnMu         sync.Mutex
	turnCancel     context.CancelFunc
	mode           string
	stream         bool
	args           types.LMGenerateArgs
	isHistoryThink bool
}

func NewLLMOpenAIApiProcessor(
	provider common.IOpenAILLMProvider, session *common.Session,
	mode string, stream bool, args types.LMGenerateArgs,
) *LLMOpenAIApiProcessor {
	if session == nil {
		session = common.NewSession(uuid.NewString(), nil)
	}
	p := &LLMOpenAIApiProcessor{
		AsyncFrameProcessor: processors.NewAsyncFrameProcessorWithPushQueueSize("LLMOpenAIApiProcessor", 1024, 1024),
		provider:            provider,
		session:             session,
		mode:                mode,
		stream:              stream,
		args:                args,
		isHistoryThink:      false,
	}

	return p
}

func (p *LLMOpenAIApiProcessor) WithIsHistoryThink(isHistoryThink bool) *LLMOpenAIApiProcessor {
	p.isHistoryThink = isHistoryThink
	return p
}

// ProcessFrame processes a frame
func (p *LLMOpenAIApiProcessor) ProcessFrame(frame frames.Frame, direction processors.FrameDirection) {
	// call frame processor to init star frame init
	p.AsyncFrameProcessor.WithPorcessFrameAllowPush(false).ProcessFrame(frame, direction)
	switch f := frame.(type) {
	case *frames.StartFrame:
		logger.Info("LLMOpenAIApiProcessor Start")
		p.PushFrame(f, direction)
	case *frames.EndFrame:
		logger.Info("LLMOpenAIApiProcessor End")
		p.PushFrame(f, direction)
	case *frames.CancelFrame:
		logger.Info("LLMOpenAIApiProcessor Cancel")
		p.PushFrame(f, direction)
	case *frames.StartInterruptionFrame:
		// Stop generating. Without this the pipeline drops the TTS audio — so
		// the caller stops hearing the reply — while the model runs the turn to
		// completion in the background. Two costs, both real: the whole reply
		// lands in the end-of-call transcript as if it had been spoken, and a
		// cancelled turn keeps consuming LLM capacity that live calls need.
		p.cancelTurn()
		// Close the turn at the point it was cut. Whatever had been handed to
		// speech by now is the honest answer to "what did the agent say"; the
		// rest was generated and never heard.
		p.session.FlushAgentTurn()
		p.PushFrame(f, direction)
	case *frames.TextFrame:
		logger.Infof("STAGE llm_recv %q", f.Text)
		switch p.mode {
		case "chat":
			p.chat(f, direction)
		case "generate":
			p.generate(f, direction)
		}
	default:
		p.QueueFrame(f, direction)
	}
}

// appendHistoryChatMessages message(api.Message) append to history list([]map[string]any)
func (p *LLMOpenAIApiProcessor) appendHistoryChatMessages(msgs []types.Message) {
	for _, msg := range msgs {
		mapMsg := map[string]any{}
		err := mapstructure.Decode(msg, &mapMsg)
		if err != nil {
			logger.Errorf("mapstructure.Decode error: %v", err)
			continue
		}
		p.session.GetChatHistory().Append(normaliseHistoryItem(mapMsg))
	}
}

// normaliseHistoryItem makes a decoded assistant turn look like the user turns
// appended elsewhere in this file: lowercase keys, plain string values.
//
// TWO separate mismatches, both silent, and together they cost a real call its
// entire agent-side transcript — five caller turns arrived and not one reply.
//
//  1. mapstructure names keys after STRUCT FIELDS, so a decoded turn has
//     "Role"/"Content" while user turns are written as literal
//     "role"/"content".
//  2. openai-go types Role as `constant.Assistant`, not `string`. Even with the
//     right key, the `item["role"].(string)` every reader does fails the type
//     assertion and yields "".
//
// Fixing it here rather than in each reader: one map with two spellings and two
// types for the same field is a trap that would be re-sprung by the next person
// to read from it. Decoding back into structs is unaffected — mapstructure
// matches field names case-insensitively and converts named string types.
func normaliseHistoryItem(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		// Any named string type (constant.Assistant and friends) becomes a
		// plain string, so a type assertion on the far side succeeds.
		if rv := reflect.ValueOf(v); rv.IsValid() && rv.Kind() == reflect.String {
			out[strings.ToLower(k)] = rv.String()
			continue
		}
		out[strings.ToLower(k)] = v
	}
	return out
}

// cancelTurn stops any generation in flight. Safe to call when none is.
func (p *LLMOpenAIApiProcessor) cancelTurn() {
	p.turnMu.Lock()
	cancel := p.turnCancel
	p.turnCancel = nil
	p.turnMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// beginTurn returns a context for one turn's generation, cancelled when the
// caller interrupts.
func (p *LLMOpenAIApiProcessor) beginTurn() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	p.turnMu.Lock()
	// A previous turn's cancel is stale by now; dropping it cannot strand a
	// generation because each turn cancels its own on the way out.
	p.turnCancel = cancel
	p.turnMu.Unlock()
	return ctx, cancel
}

func (p *LLMOpenAIApiProcessor) chat(frame *frames.TextFrame, direction processors.FrameDirection) {
	chatHistory := p.session.GetChatHistory()
	chatHistory.Append(map[string]any{"role": "user", "content": frame.Text})
	historyList := chatHistory.ToListWithoutTools() // init tools in provider
	messages := make([]types.Message, 0)
	err := mapstructure.Decode(historyList, &messages)
	if err != nil {
		logger.Error("chat", "err", err)
	}

	// Time the whole turn, from the caller's utterance being handed to the LLM
	// to the first token we can speak. Started outside the tool loop on
	// purpose: a turn that calls a tool makes several requests and the caller
	// waits for all of them, so timing each request separately would call a
	// turn fast that the caller heard as a long silence.
	turnCtx, endTurn := p.beginTurn()
	defer endTurn()
	// A turn that ends normally is reported here; one cut short is reported by
	// the interruption handler. Flushing twice is harmless — the second finds
	// nothing left.
	defer p.session.FlushAgentTurn()

	turnStart := time.Now()
	turnObserved := false
	observeTurn := func() {
		if turnObserved {
			return
		}
		turnObserved = true
		p.session.ObserveLLMTTFT(time.Since(turnStart))
	}

	isToolCalls := true
	cnToolCalls := 0
	for isToolCalls {
		if cnToolCalls > 3 {
			logger.Error("chat", "err", "too many tool calls")
			break
		}
		cnToolCalls++
		if !p.stream {
			p.provider.Chat(turnCtx, p.args, messages, func(resp *openai.ChatCompletion) error {
				toolMsgs := []types.Message{}
				for i, toolCall := range resp.Choices[0].Message.ToolCalls {
					// Extract the location from the function call arguments
					funcArgs := strings.ReplaceAll(toolCall.Function.Arguments, "{}", "")
					resp.Choices[0].Message.ToolCalls[i].Function.Arguments = funcArgs

					var args map[string]any
					err := json.Unmarshal([]byte(funcArgs), &args)
					if err != nil {
						logger.Errorf("Failed to unmarshal function arguments: %v err: %v", funcArgs, err)
						continue
					}
					result, err := p.execFunc(toolCall.Function.Name, args)
					if err != nil {
						logger.Errorf("Failed to execute function: %v err: %v", toolCall.Function.Name, err)
						continue
					}
					toolMsgs = append(toolMsgs, types.Message{
						ChatCompletionMessage: openai.ChatCompletionMessage{Role: "tool", Content: result},
						ToolCallID:            toolCall.ID,
					})
					p.QueueFrame(achatbot_frames.NewFunctionCallFrame(toolCall.ID, toolCall.Function.Name, args, i), direction)
				}
				// If there is a was a function call, continue the conversation
				if len(toolMsgs) > 0 { //call_tools
					if !p.isHistoryThink {
						resp.Choices[0].Message.Reasoning = ""
					}
					msg := types.Message{ChatCompletionMessage: resp.Choices[0].Message}
					messages = append(messages, msg)
					p.appendHistoryChatMessages([]types.Message{msg})
					messages = append(messages, toolMsgs...)
					p.appendHistoryChatMessages(toolMsgs)
					isToolCalls = true
				}
				if resp.Choices[0].Message.Reasoning != "" {
					p.QueueFrame(achatbot_frames.NewThinkTextFrame(resp.Choices[0].Message.Reasoning), direction)
				}
				if resp.Choices[0].Message.Content != "" {
					observeTurn()
					p.session.RecordAgentChunk(resp.Choices[0].Message.Content)
					isToolCalls = false
					if !p.isHistoryThink {
						resp.Choices[0].Message.Reasoning = ""
					}
					msg := types.Message{ChatCompletionMessage: resp.Choices[0].Message}
					messages = append(messages, msg)
					p.appendHistoryChatMessages([]types.Message{msg})
					p.QueueFrame(frames.NewTextFrame(resp.Choices[0].Message.Content), direction)
				}
				return nil
			})
		} else { //stream
			acc := openai.ChatCompletionAccumulator{}
			toolMsgs := []types.Message{}
			firstToken := true
			p.provider.ChatStream(turnCtx, p.args, messages, func(chunk *openai.ChatCompletionChunk) error {
				acc.AddChunk(*chunk)
				if len(chunk.Choices) == 0 {
					return nil
				}

				if chunk.Choices[0].Delta.Reasoning != "" {
					p.QueueFrame(achatbot_frames.NewThinkTextFrame(chunk.Choices[0].Delta.Reasoning), direction)
				}
				if chunk.Choices[0].Delta.Content != "" {
					if firstToken {
						logger.Infof("STAGE llm_first_token")
						firstToken = false
						observeTurn()
					}
					// Record it at the moment it is handed downstream to be
					// spoken. Reading the finished message off chat history
					// instead reports everything the model produced, which
					// after an interruption is not what the caller heard.
					p.session.RecordAgentChunk(chunk.Choices[0].Delta.Content)
					p.QueueFrame(frames.NewTextFrame(chunk.Choices[0].Delta.Content), direction)
				}

				if chunk.Choices[0].Delta.ToolCalls != nil {
					for _, tool := range chunk.Choices[0].Delta.ToolCalls {
						tool.Function.Arguments = strings.ReplaceAll(tool.Function.Arguments, "{}", "")
						var args map[string]any
						err := json.Unmarshal([]byte(tool.Function.Arguments), &args)
						if err != nil {
							logger.Errorf("Failed to Unmarshal err: %v", err)
							continue
						}
						result, err := p.execFunc(tool.Function.Name, args)
						if err != nil {
							logger.Error("Execute", "err", err, "funcName", tool.Function.Name, "funcArgs", tool.Function.Arguments)
							continue
						}
						toolMsgs = append(toolMsgs, types.Message{
							ChatCompletionMessage: openai.ChatCompletionMessage{Role: "tool", Content: result},
							ToolCallID:            tool.ID,
						})
						p.QueueFrame(achatbot_frames.NewFunctionCallFrame(tool.ID, tool.Function.Name, args, int(tool.Index)), direction)
					}
				}
				return nil
			})
			// If there is a was a function call, continue the conversation
			if len(toolMsgs) > 0 { //call_tools
				if !p.isHistoryThink {
					acc.Choices[0].Message.Reasoning = ""
				}
				msg := types.Message{ChatCompletionMessage: acc.Choices[0].Message}
				messages = append(messages, msg)
				p.appendHistoryChatMessages([]types.Message{msg})
				messages = append(messages, toolMsgs...)
				p.appendHistoryChatMessages(toolMsgs)
				isToolCalls = true
				cnToolCalls++
			}

			if len(acc.Choices) > 0 && acc.Choices[0].Message.Content != "" {
				if !p.isHistoryThink {
					acc.Choices[0].Message.Reasoning = ""
				}
				msg := types.Message{ChatCompletionMessage: acc.Choices[0].Message}
				messages = append(messages, msg)
				p.appendHistoryChatMessages([]types.Message{msg})
				isToolCalls = false
			}
		} //end stream
	} //end call

	p.QueueFrame(achatbot_frames.NewTurnEndFrame(), direction)
	logger.Infof("ChatHistory: %+v", p.session.GetChatHistory().ToList())
	p.session.IncrementChatRound()
}

func (p *LLMOpenAIApiProcessor) generate(frame *frames.TextFrame, direction processors.FrameDirection) {
	if !p.stream {
		p.provider.Generate(context.Background(), p.args, frame.Text, func(resp *openai.Completion) error {
			if resp.Choices[0].Text != "" {
				p.QueueFrame(frames.NewTextFrame(resp.Choices[0].Text), direction)
			}
			return nil
		})
	} else {
		p.provider.GenerateStream(context.Background(), p.args, frame.Text, func(resp *openai.Completion) error {
			if resp.Choices[0].Text != "" {
				p.QueueFrame(frames.NewTextFrame(resp.Choices[0].Text), direction)
			}
			return nil
		})
	}
	p.QueueFrame(achatbot_frames.NewTurnEndFrame(), direction)
}

// execFunc runs a tool, preferring an implementation bound to this session.
//
// Session-scoped tools shadow global ones because a tool that acts on the call
// it was invoked from — transferring it, ending it — cannot be a process-wide
// singleton: the invocation carries only arguments, so a global handler has no
// way to tell which of the calls in flight it belongs to.
func (p *LLMOpenAIApiProcessor) execFunc(name string, args map[string]any) (string, error) {
	if fn := p.session.Func(name); fn != nil {
		return fn.Execute(args)
	}
	return functions.RegisterFuncs.Execute(name, args)
}
