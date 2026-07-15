package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenRouter's chat completions endpoint is OpenAI-compatible; these are
// hand-written request/response types carrying only the fields we use, in
// place of the generated SDK's exhaustive union types.
const openRouterURL = "https://openrouter.ai/api/v1/chat/completions"

// receiptModelTimeout bounds one vision-model call. Receipts run through a
// synchronous handler, so a hung provider must fail the command, not wedge it.
const receiptModelTimeout = 60 * time.Second

type orRequest struct {
	Model    string      `json:"model"`
	Messages []orMessage `json:"messages"`
	// No omitempty: temperature 0 is a deliberate value, not an unset field.
	Temperature    float64           `json:"temperature"`
	MaxTokens      int               `json:"max_tokens"`
	ResponseFormat *orResponseFormat `json:"response_format,omitempty"`
}

type orResponseFormat struct {
	Type string `json:"type"`
}

type orMessage struct {
	Role string `json:"role"`
	// string for the system message, []orContentPart for the multimodal
	// user message.
	Content any `json:"content"`
}

type orContentPart struct {
	Type     string      `json:"type"` // "text" or "image_url"
	Text     string      `json:"text,omitempty"`
	ImageURL *orImageURL `json:"image_url,omitempty"`
}

type orImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type orResponse struct {
	Choices []orChoice `json:"choices"`
	// OpenRouter reports some failures (moderation, provider errors) in a
	// 200 body rather than an HTTP status.
	Error *orError `json:"error"`
}

type orError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type orChoice struct {
	FinishReason string `json:"finish_reason"`
	Message      struct {
		Content string  `json:"content"`
		Refusal *string `json:"refusal"`
	} `json:"message"`
}

// processReceiptImage sends the receipt photo plus the user's caption to the
// vision model and returns the model's reply, which the system prompt and
// json_object response format constrain to a bare JSON object. Every failure
// mode ends in an error — never a partial result.
func processReceiptImage(ctx context.Context, imgBytes []byte, imgDescr string, cfg *Config) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, receiptModelTimeout)
	defer cancel()

	mime := http.DetectContentType(imgBytes)
	dataURL := fmt.Sprintf("data:%s;base64,%s", mime, base64.StdEncoding.EncodeToString(imgBytes))
	body, err := json.Marshal(orRequest{
		Model: cfg.VisionModel,
		Messages: []orMessage{
			{Role: "system", Content: receiptParserPrompt},
			{Role: "user", Content: []orContentPart{
				{Type: "text", Text: "Additional Context For Image: " + imgDescr},
				{Type: "image_url", ImageURL: &orImageURL{URL: dataURL, Detail: "high"}},
			}},
		},
		Temperature: 0,
		// The reply is a small fixed-shape JSON object (~120 tokens worst
		// case); the cap stops a misbehaving model from rambling on our
		// dime. Overruns surface as finish_reason=length below.
		MaxTokens:      300,
		ResponseFormat: &orResponseFormat{Type: "json_object"},
	})
	if err != nil {
		return "", fmt.Errorf("marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build chat request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.OpenAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("openrouter chat send: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read chat response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openrouter status %s: %s", resp.Status, errSnippet(respBody))
	}

	var out orResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("decode chat response: %w", err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("openrouter error %d: %s", out.Error.Code, out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("openrouter returned no choices")
	}
	choice := out.Choices[0]
	// Truncated output means unparseable JSON; fail here rather than letting
	// json.Unmarshal surface a confusing syntax error downstream.
	if choice.FinishReason == "length" {
		return "", errors.New("model output truncated (finish_reason=length)")
	}
	if choice.Message.Refusal != nil && *choice.Message.Refusal != "" {
		return "", fmt.Errorf("model refused: %s", *choice.Message.Refusal)
	}
	if choice.Message.Content == "" {
		return "", fmt.Errorf("assistant message has no content (finish_reason=%s)", choice.FinishReason)
	}
	return stripJSONFences(choice.Message.Content), nil
}

// errSnippet trims an error response body for inclusion in an error message.
func errSnippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// stripJSONFences unwraps a markdown code fence ("```json\n{...}\n```") if
// the model added one despite json_object mode and the prompt.
func stripJSONFences(s string) string {
	s = strings.TrimSpace(s)
	if after, ok := strings.CutPrefix(s, "```json"); ok {
		s = after
	} else if after, ok := strings.CutPrefix(s, "```"); ok {
		s = after
	} else {
		return s
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

const receiptParserPrompt = `
You are a receipt parser for a personal finance bot. Extract transaction details from the receipt image and any user-provided context.
Respond with ONLY a JSON object, no markdown fences, no commentary.
The user may provide additional context about the transaction in the user prompt
Use the receipt image as the primary source of truth. Use the user's context to fill gaps or confirm details. If no image is provided, use the user's context alone.
## JSON Schema
{
"merchant": "string — store/business name",
"amount": 0.00,
"primary_category": "string — one of the allowed primary categories",
"detailed_category": "string — one of the allowed detailed categories under that primary"
}
## Allowed Categories
PRIMARY: FOOD_AND_DRINK
*   FOOD_AND_DRINK_BEER_WINE_AND_LIQUOR
*   FOOD_AND_DRINK_COFFEE
*   FOOD_AND_DRINK_FAST_FOOD
*   FOOD_AND_DRINK_GROCERIES
*   FOOD_AND_DRINK_RESTAURANT
*   FOOD_AND_DRINK_VENDING_MACHINES
*   FOOD_AND_DRINK_OTHER_FOOD_AND_DRINK
PRIMARY: GENERAL_MERCHANDISE
*   GENERAL_MERCHANDISE_BOOKSTORES_AND_NEWSSTANDS
*   GENERAL_MERCHANDISE_CLOTHING_AND_ACCESSORIES
*   GENERAL_MERCHANDISE_CONVENIENCE_STORES
*   GENERAL_MERCHANDISE_DEPARTMENT_STORES
*   GENERAL_MERCHANDISE_DISCOUNT_STORES
*   GENERAL_MERCHANDISE_ELECTRONICS
*   GENERAL_MERCHANDISE_GIFTS_AND_NOVELTIES
*   GENERAL_MERCHANDISE_OFFICE_SUPPLIES
*   GENERAL_MERCHANDISE_ONLINE_MARKETPLACES
*   GENERAL_MERCHANDISE_PET_SUPPLIES
*   GENERAL_MERCHANDISE_SPORTING_GOODS
*   GENERAL_MERCHANDISE_SUPERSTORES
*   GENERAL_MERCHANDISE_TOBACCO_AND_VAPE
*   GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE
PRIMARY: ENTERTAINMENT
*   ENTERTAINMENT_CASINOS_AND_GAMBLING
*   ENTERTAINMENT_MUSIC_AND_AUDIO
*   ENTERTAINMENT_SPORTING_EVENTS_AMUSEMENT_PARKS_AND_MUSEUMS
*   ENTERTAINMENT_TV_AND_MOVIES
*   ENTERTAINMENT_VIDEO_GAMES
*   ENTERTAINMENT_OTHER_ENTERTAINMENT
PRIMARY: HOME_IMPROVEMENT
*   HOME_IMPROVEMENT_FURNITURE
*   HOME_IMPROVEMENT_HARDWARE
*   HOME_IMPROVEMENT_REPAIR_AND_MAINTENANCE
*   HOME_IMPROVEMENT_SECURITY
*   HOME_IMPROVEMENT_OTHER_HOME_IMPROVEMENT
PRIMARY: MEDICAL
*   MEDICAL_DENTAL_CARE
*   MEDICAL_EYE_CARE
*   MEDICAL_NURSING_CARE
*   MEDICAL_PHARMACIES_AND_SUPPLEMENTS
*   MEDICAL_PRIMARY_CARE
*   MEDICAL_VETERINARY_SERVICES
*   MEDICAL_OTHER_MEDICAL
PRIMARY: PERSONAL_CARE
*   PERSONAL_CARE_GYMS_AND_FITNESS_CENTERS
*   PERSONAL_CARE_HAIR_AND_BEAUTY
*   PERSONAL_CARE_LAUNDRY_AND_DRY_CLEANING
*   PERSONAL_CARE_OTHER_PERSONAL_CARE
PRIMARY: GENERAL_SERVICES
*   GENERAL_SERVICES_ACCOUNTING_AND_FINANCIAL_PLANNING
*   GENERAL_SERVICES_AUTOMOTIVE
*   GENERAL_SERVICES_CHILDCARE
*   GENERAL_SERVICES_CONSULTING_AND_LEGAL
*   GENERAL_SERVICES_EDUCATION
*   GENERAL_SERVICES_INSURANCE
*   GENERAL_SERVICES_POSTAGE_AND_SHIPPING
*   GENERAL_SERVICES_STORAGE
*   GENERAL_SERVICES_OTHER_GENERAL_SERVICES
PRIMARY: GOVERNMENT_AND_NON_PROFIT
*   GOVERNMENT_AND_NON_PROFIT_DONATIONS
*   GOVERNMENT_AND_NON_PROFIT_GOVERNMENT_DEPARTMENTS_AND_AGENCIES
*   GOVERNMENT_AND_NON_PROFIT_TAX_PAYMENT
*   GOVERNMENT_AND_NON_PROFIT_OTHER_GOVERNMENT_AND_NON_PROFIT
PRIMARY: TRANSPORTATION
*   TRANSPORTATION_BIKES_AND_SCOOTERS
*   TRANSPORTATION_GAS
*   TRANSPORTATION_PARKING
*   TRANSPORTATION_PUBLIC_TRANSIT
*   TRANSPORTATION_TAXIS_AND_RIDE_SHARES
*   TRANSPORTATION_TOLLS
*   TRANSPORTATION_OTHER_TRANSPORTATION
PRIMARY: TRAVEL
*   TRAVEL_FLIGHTS
*   TRAVEL_LODGING
*   TRAVEL_RENTAL_CARS
*   TRAVEL_OTHER_TRAVEL
PRIMARY: RENT_AND_UTILITIES
*   RENT_AND_UTILITIES_GAS_AND_ELECTRICITY
*   RENT_AND_UTILITIES_INTERNET_AND_CABLE
*   RENT_AND_UTILITIES_RENT
*   RENT_AND_UTILITIES_SEWAGE_AND_WASTE_MANAGEMENT
*   RENT_AND_UTILITIES_TELEPHONE
*   RENT_AND_UTILITIES_WATER
*   RENT_AND_UTILITIES_OTHER_UTILITIES
PRIMARY: BANK_FEES
*   BANK_FEES_ATM_FEES
*   BANK_FEES_FOREIGN_TRANSACTION_FEES
*   BANK_FEES_INSUFFICIENT_FUNDS
*   BANK_FEES_INTEREST_CHARGE
*   BANK_FEES_OVERDRAFT_FEES
*   BANK_FEES_OTHER_BANK_FEES
PRIMARY: LOAN_PAYMENTS
*   LOAN_PAYMENTS_CAR_PAYMENT
*   LOAN_PAYMENTS_CREDIT_CARD_PAYMENT
*   LOAN_PAYMENTS_PERSONAL_LOAN_PAYMENT
*   LOAN_PAYMENTS_MORTGAGE_PAYMENT
*   LOAN_PAYMENTS_STUDENT_LOAN_PAYMENT
*   LOAN_PAYMENTS_OTHER_PAYMENT
PRIMARY: TRANSFER_OUT
*   TRANSFER_OUT_INVESTMENT_AND_RETIREMENT_FUNDS
*   TRANSFER_OUT_SAVINGS
*   TRANSFER_OUT_WITHDRAWAL
*   TRANSFER_OUT_ACCOUNT_TRANSFER
*   TRANSFER_OUT_OTHER_TRANSFER_OUT
`
