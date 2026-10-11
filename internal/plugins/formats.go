package plugins

// Converter-owned metadata keeps engine format tracking independent of IDs.
func (*OpenAI2Anthropic) RequestFormat() string     { return "anthropic" }
func (*OpenAI2Anthropic) ResponseFormat() string    { return "openai" }
func (*Anthropic2OpenAI) RequestFormat() string     { return "openai" }
func (*Anthropic2OpenAI) ResponseFormat() string    { return "anthropic" }
func (*Responses2OpenAI) RequestFormat() string     { return "openai" }
func (*Responses2OpenAI) ResponseFormat() string    { return "openai_responses" }
func (*OpenAI2Responses) RequestFormat() string     { return "openai_responses" }
func (*OpenAI2Responses) ResponseFormat() string    { return "openai" }
func (*Responses2Anthropic) RequestFormat() string  { return "anthropic" }
func (*Responses2Anthropic) ResponseFormat() string { return "openai_responses" }
func (*Anthropic2Responses) RequestFormat() string  { return "openai_responses" }
func (*Anthropic2Responses) ResponseFormat() string { return "anthropic" }
func (*OpenAI2Gemini) RequestFormat() string        { return "gemini" }
func (*OpenAI2Gemini) ResponseFormat() string       { return "openai" }
func (*Anthropic2Gemini) RequestFormat() string     { return "gemini" }
func (*Anthropic2Gemini) ResponseFormat() string    { return "anthropic" }
func (*Gemini2OpenAI) RequestFormat() string        { return "openai" }
func (*Gemini2OpenAI) ResponseFormat() string       { return "gemini" }
func (*Gemini2Anthropic) RequestFormat() string     { return "anthropic" }
func (*Gemini2Anthropic) ResponseFormat() string    { return "gemini" }
func (*Gemini2Responses) RequestFormat() string     { return "openai_responses" }
func (*Gemini2Responses) ResponseFormat() string    { return "gemini" }
