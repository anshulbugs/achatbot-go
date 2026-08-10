package llm

//https://github.com/openai/openai-go

import (
	"achatbot/pkg/common"
	"achatbot/pkg/modules/functions"
	"achatbot/pkg/types"
	"context"
	"net/http"
	"os"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
	"github.com/openai/openai-go/v3/shared/constant"
	"github.com/weedge/pipeline-go/pkg/logger"
)

type OpenAIAPIProvider struct {
	name   string
	model  string
	client openai.Client
	tools  []openai.ChatCompletionToolUnionParam
}

const (
	OpenAIAPIProviderName    = "openai_api"
	OpenAIAPIProviderBaseUrl = "https://api.openai.com/v1"

	OllamaAPIProviderBaseUrl = "http://127.0.0.1:11434/v1"

	OpenRouterAIAPIProviderBaseUrl               = "https://openrouter.ai/api/v1"
	OpenRouterAIAPIProviderModelQwen3_235b_free  = "qwen/qwen3-235b-a22b:free" //have some issue,don't match openaiapi(think&tools)
	OpenRouterAIAPIProviderModelQwen2_5_72b_free = "qwen/qwen-2.5-72b-instruct:free"
)

// HTTPClient, when set, is used by every OpenAI-compatible client built here.
//
// The hook exists so the application can wrap the transport — routing headers,
// timing — without this package knowing what any of that is for. Set it before
// the provider pool is constructed: the client is captured at construction and
// a later change has no effect on providers already built.
var HTTPClient *http.Client

func NewOpenAIAPIProvider(name, baseUrl, model string, toolNames []string) *OpenAIAPIProvider {
	apiKey := os.Getenv("OPENAI_API_KEY")
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseUrl),
	}
	if HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(HTTPClient))
	}
	client := openai.NewClient(opts...)

	tools := []openai.ChatCompletionToolUnionParam{}
	if len(toolNames) > 0 {
		mapTools := functions.RegisterFuncs.GetToolCallsByName(toolNames)
		toolSchema, err := functions.AdapteOpenAIToolSchema(mapTools)
		if err != nil {
			logger.Error("NewOpenAIAPIProvider failed with Tools", "error", err)
			return nil
		}
		tools = toolSchema
		logger.Infof("use Tools: %v", toolNames)
	}
	p := &OpenAIAPIProvider{
		name:   name,
		model:  model,
		client: client,
		tools:  tools,
	}

	return p
}

// AddTools advertises additional tool schemas to the model beyond the ones
// named in config.
//
// The config list is resolved from the global function registry at
// construction, which cannot express a tool that exists only for one call —
// transferring THIS call, for example. Such tools are bound per session, and
// this is how the model gets told they exist. Registering an implementation
// without advertising it is a silent no-op: the model never learns the tool is
// available, so it never calls it.
func (p *OpenAIAPIProvider) AddTools(schemas []map[string]any) {
	if len(schemas) == 0 {
		return
	}
	adapted, err := functions.AdapteOpenAIToolSchema(schemas)
	if err != nil {
		logger.Error("AddTools: schema adaptation failed", "error", err)
		return
	}
	p.tools = append(p.tools, adapted...)
}

// Generate 生成文本token
// call /v1/completions
func (p *OpenAIAPIProvider) Generate(ctx context.Context, args types.LMGenerateArgs, prompt string, respFunc common.OpenAICompletionRespFunc) {
	completion, err := p.client.Completions.New(
		ctx, openai.CompletionNewParams{
			Prompt:           openai.CompletionNewParamsPromptUnion{OfString: param.Opt[string]{Value: prompt}},
			Model:            openai.CompletionNewParamsModel(p.model),
			N:                param.Opt[int64]{Value: args.LmN},
			Seed:             param.Opt[int64]{Value: args.LmGenSeed},
			MaxTokens:        param.Opt[int64]{Value: args.LmMaxTokens},
			FrequencyPenalty: param.Opt[float64]{Value: args.LmGenFrequencyPenalty},
			Temperature:      param.Opt[float64]{Value: args.LmGenTemperature},
			TopP:             param.Opt[float64]{Value: args.LmGenTopP},
			Stop:             openai.CompletionNewParamsStopUnion{OfStringArray: args.LmGenStops},
		},
		// Override the header
		option.WithHeader("HTTP-Referer", "github.com/weedge"),
		option.WithHeader("X-Title", "achatbot-go"),
		option.WithMaxRetries(2), // Override the default max retries
	)
	if err != nil {
		logger.Errorf("Generate failed: %v", err)
		return
	}
	logger.Infof("%+s", completion.RawJSON())

	err = respFunc(completion)
	if err != nil {
		logger.Errorf("Generate failed: %v", err)
		return
	}
}

func (p *OpenAIAPIProvider) convertMessages(messages []types.Message) []openai.ChatCompletionMessageParamUnion {
	msgUnion := []openai.ChatCompletionMessageParamUnion{}

	for _, msg := range messages {
		switch msg.Role {
		case "system":
			msgUnion = append(msgUnion, openai.SystemMessage(msg.Content))
		case "user":
			msgUnion = append(msgUnion, openai.UserMessage(msg.Content))
		case "assistant":
			if msg.Content != "" {
				msgUnion = append(msgUnion, openai.AssistantMessage(msg.Content))
			}
			if msg.ToolCalls != nil { // tool_calls
				toolCalls := []openai.ChatCompletionMessageToolCallUnionParam{}
				for _, toolCall := range msg.ToolCalls {
					toolCalls = append(
						toolCalls,
						openai.ChatCompletionMessageToolCallUnionParam{
							OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
								ID:   toolCall.ID,
								Type: constant.Function(toolCall.Type),
								Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
									Name:      toolCall.Function.Name,
									Arguments: toolCall.Function.Arguments,
								},
							},
						},
					)
				}
				if len(toolCalls) > 0 {
					msgUnion = append(msgUnion, openai.ChatCompletionMessageParamUnion{OfAssistant: &openai.ChatCompletionAssistantMessageParam{
						Role:      msg.Role,
						ToolCalls: toolCalls,
					}})
				}
			}
		case "tool":
			msgUnion = append(msgUnion, openai.ChatCompletionMessageParamUnion{
				OfTool: &openai.ChatCompletionToolMessageParam{
					Role:       constant.Tool(msg.Role),
					ToolCallID: msg.ToolCallID,
					Content:    openai.ChatCompletionToolMessageParamContentUnion{OfString: param.Opt[string]{Value: msg.Content}},
				},
			})
		}
	}

	return msgUnion
}

func (p *OpenAIAPIProvider) getChatCompletionNewParams(messages []types.Message, args types.LMGenerateArgs) openai.ChatCompletionNewParams {
	params := openai.ChatCompletionNewParams{
		Messages:            p.convertMessages(messages),
		Tools:               p.tools,
		Model:               shared.ChatModel(p.model),
		PromptCacheKey:      param.Opt[string]{Value: args.PromptCacheKey},
		N:                   param.Opt[int64]{Value: args.LmN},
		Seed:                param.Opt[int64]{Value: args.LmGenSeed},
		MaxTokens:           param.Opt[int64]{Value: args.LmMaxTokens},
		MaxCompletionTokens: param.Opt[int64]{Value: args.LmGenMaxTokens},
		FrequencyPenalty:    param.Opt[float64]{Value: args.LmGenFrequencyPenalty},
		Temperature:         param.Opt[float64]{Value: args.LmGenTemperature},
		TopP:                param.Opt[float64]{Value: args.LmGenTopP},
		Stop:                openai.ChatCompletionNewParamsStopUnion{OfStringArray: args.LmGenStops},
	}
	if p.name == OpenAIAPIProviderName { //think for openai(the same as)
		if args.LmGenThinking != nil {
			switch *args.LmGenThinking {
			case "minimal":
				params.ReasoningEffort = shared.ReasoningEffortMinimal
			case "low":
				params.ReasoningEffort = shared.ReasoningEffortLow
			case "medium":
				params.ReasoningEffort = shared.ReasoningEffortMedium
			case "high":
				params.ReasoningEffort = shared.ReasoningEffortHigh
			default:
				params.ReasoningEffort = shared.ReasoningEffortMinimal
			}
		}
	}
	return params
}

// Chat 上下文chat_template 指令生成文本token
// call /v1/chat/completions
func (p *OpenAIAPIProvider) Chat(ctx context.Context,
	args types.LMGenerateArgs, messages []types.Message,
	respFunc common.OpenAIChatCompletionRespFunc,
) {
	params := p.getChatCompletionNewParams(messages, args)
	chatCompletion, err := p.client.Chat.Completions.New(ctx, params,
		// Override the header
		option.WithHeader("HTTP-Referer", "github.com/weedge"),
		option.WithHeader("X-Title", "achatbot-go"),
		option.WithMaxRetries(2), // Override the default max retries
	)
	if err != nil {
		logger.Infof("Chat failed: %v", err)
		return
	}
	logger.Infof("%s", chatCompletion.RawJSON())

	err = respFunc(chatCompletion)
	if err != nil {
		logger.Infof("Chat failed: %v", err)
		return
	}
}

// GenerateStream stream generate 生成文本token
func (p *OpenAIAPIProvider) GenerateStream(ctx context.Context, args types.LMGenerateArgs, prompt string, respFunc common.OpenAIStreamCompletionRespFunc) {
	stream := p.client.Completions.NewStreaming(
		ctx, openai.CompletionNewParams{
			Prompt:           openai.CompletionNewParamsPromptUnion{OfString: param.Opt[string]{Value: prompt}},
			N:                param.Opt[int64]{Value: args.LmN},
			Seed:             param.Opt[int64]{Value: args.LmGenSeed},
			MaxTokens:        param.Opt[int64]{Value: args.LmMaxTokens},
			FrequencyPenalty: param.Opt[float64]{Value: args.LmGenFrequencyPenalty},
			Temperature:      param.Opt[float64]{Value: args.LmGenTemperature},
			TopP:             param.Opt[float64]{Value: args.LmGenTopP},
			Stop:             openai.CompletionNewParamsStopUnion{OfStringArray: args.LmGenStops},
		},

		// Override the header
		option.WithHeader("HTTP-Referer", "github.com/weedge"),
		option.WithHeader("X-Title", "achatbot-go"),
		option.WithMaxRetries(2), // Override the default max retries
	)
	for stream.Next() {
		chunk := stream.Current()
		err := respFunc(&chunk)
		if err != nil {
			logger.Errorf("generate stream error: %v", err)
		}
	}
}

// ChatStream stream chat 上下文chat_template 指令生成文本token
func (p *OpenAIAPIProvider) ChatStream(ctx context.Context, args types.LMGenerateArgs, messages []types.Message, respFunc common.OpenAIStreamChatCompletionRespFunc) {

	params := p.getChatCompletionNewParams(messages, args)
	stream := p.client.Chat.Completions.NewStreaming(ctx, params,
		// Override the header
		option.WithHeader("HTTP-Referer", "github.com/weedge"),
		option.WithHeader("X-Title", "achatbot-go"),
		option.WithMaxRetries(2), // Override the default max retries
	)

	for stream.Next() {
		chunk := stream.Current()
		err := respFunc(&chunk)
		if err != nil {
			logger.Errorf("chat stream error: %v", err)
		}
	}

	if stream.Err() != nil {
		logger.Errorf("chat stream error: %v", stream.Err())
		return
	}
}

func (p *OpenAIAPIProvider) Name() string {
	return p.name
}

// ChatStreamWithoutTools runs one streamed turn with the tool list omitted.
//
// A LAST RESORT, not an alternative path. gemma-4 served by SGLang returns a
// completion with no content, no tool call and finish_reason=stop a variable
// fraction of the time — rare on an idle box, repeatable under call load, and
// measured at three-in-twelve on one real conversation's history. Retrying the
// identical request usually clears it; when it does not, the caller is left
// listening to silence.
//
// Dropping the tools is what reliably breaks the pattern: the same history that
// produced three empty completions in a row produced text on twelve of twelve
// attempts with the tools omitted. The turn loses the ability to CALL a tool,
// which is the right thing to give up — a caller who hears a sentence and has to
// ask again is in a far better position than one who hears nothing at all, and
// the transfer path has its own repair for a model that talks about a handover
// instead of invoking it.
func (p *OpenAIAPIProvider) ChatStreamWithoutTools(ctx context.Context, args types.LMGenerateArgs, messages []types.Message, respFunc common.OpenAIStreamChatCompletionRespFunc) {
	params := p.getChatCompletionNewParams(messages, args)
	params.Tools = nil

	stream := p.client.Chat.Completions.NewStreaming(ctx, params,
		option.WithHeader("HTTP-Referer", "github.com/weedge"),
		option.WithHeader("X-Title", "achatbot-go"),
		option.WithMaxRetries(2),
	)
	for stream.Next() {
		chunk := stream.Current()
		if err := respFunc(&chunk); err != nil {
			logger.Errorf("chat stream (no tools) error: %v", err)
		}
	}
	if stream.Err() != nil {
		logger.Errorf("chat stream (no tools) error: %v", stream.Err())
	}
}

// ChatStreamForcingTools runs one streamed turn with tool_choice="required", so
// the model must answer with a tool call rather than prose.
//
// THE REPAIR FOR A FAILED TOOL CALL. An "empty completion" from this stack --
// no content, no tool call, finish_reason=stop -- only ever happens on a turn
// where the model was trying to invoke a tool: every one in the logs sits on a
// transfer request or immediately after a transfer executed, and never anywhere
// else. The model emits tool-call markup and SGLang's parser fails to turn it
// into a tool_calls field, so what arrives is a response with nothing in it.
//
// Forcing the choice removes the ambiguity the parser is failing on. Measured
// on the history of a call that produced empties: tool_choice=auto returned a
// tool call nine times in ten, tool_choice=required ten times in ten.
//
// Only correct as a RETRY. Forcing on the first attempt would make the model
// call a tool on turns where it should simply be talking.
func (p *OpenAIAPIProvider) ChatStreamForcingTools(ctx context.Context, args types.LMGenerateArgs, messages []types.Message, respFunc common.OpenAIStreamChatCompletionRespFunc) {
	params := p.getChatCompletionNewParams(messages, args)
	if len(params.Tools) == 0 {
		return
	}
	params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
		OfAuto: param.Opt[string]{Value: "required"},
	}

	stream := p.client.Chat.Completions.NewStreaming(ctx, params,
		option.WithHeader("HTTP-Referer", "github.com/weedge"),
		option.WithHeader("X-Title", "achatbot-go"),
		option.WithMaxRetries(2),
	)
	for stream.Next() {
		chunk := stream.Current()
		if err := respFunc(&chunk); err != nil {
			logger.Errorf("chat stream (forced tools) error: %v", err)
		}
	}
	if stream.Err() != nil {
		logger.Errorf("chat stream (forced tools) error: %v", stream.Err())
	}
}

// HasTools reports whether any tool is advertised on this provider.
func (p *OpenAIAPIProvider) HasTools() bool { return len(p.tools) > 0 }
