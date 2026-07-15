package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

// processReceiptImage sends the receipt photo plus the user's caption to the
// vision model and returns the model's reply, which the system prompt
// constrains to a bare JSON object. Every response shape the SDK can produce
// is funnelled to either that JSON string or an error — never a partial
// result.
func processReceiptImage(ctx context.Context, imgBytes []byte, imgDescr string, cfg *Config) (string, error) {
	s := openrouter.New(
		openrouter.WithSecurity(cfg.OpenAPIKey),
	)
	imgB64 := base64.StdEncoding.EncodeToString(imgBytes)
	mime := http.DetectContentType(imgBytes)
	dataURL := fmt.Sprintf("data:%s;base64,%s", mime, imgB64)

	res, err := s.Chat.Send(ctx, components.ChatRequest{
		Model: openrouter.Pointer(cfg.VisionModel),
		Messages: []components.ChatMessages{
			components.CreateChatMessagesSystem(components.ChatSystemMessage{
				Content: components.CreateChatSystemMessageContentStr(systemPrompt),
			}),
			components.CreateChatMessagesUser(components.ChatUserMessage{
				Role: components.ChatUserMessageRoleUser,
				Content: components.CreateChatUserMessageContentArrayOfChatContentItems(
					[]components.ChatContentItems{
						components.CreateChatContentItemsText(components.ChatContentText{
							Type: components.ChatContentTextTypeText,
							Text: fmt.Sprintf("Additional Context For Image: %s", imgDescr),
						}),
						components.CreateChatContentItemsImageURL(components.ChatContentImage{
							Type: components.ChatContentImageTypeImageURL,
							ImageURL: components.ChatContentImageImageURL{
								Detail: components.ChatContentImageDetailHigh.ToPointer(),
								URL:    dataURL,
							},
						}),
					},
				),
			}),
		},
		Temperature: optionalnullable.From(openrouter.Pointer(0.0)),
		// The reply is a small fixed-shape JSON object (~120 tokens worst
		// case); the cap stops a misbehaving model from rambling on our dime.
		// Overruns surface as finish_reason=length and are rejected above.
		MaxTokens: optionalnullable.From(openrouter.Pointer(int64(300))),
	}, nil)
	if err != nil {
		return "", fmt.Errorf("openrouter chat send: %w", err)
	}
	if res == nil || res.ChatResult == nil {
		return "", errors.New("openrouter returned no chat result")
	}
	if len(res.ChatResult.Choices) == 0 {
		return "", errors.New("openrouter returned no choices")
	}
	choice := res.ChatResult.Choices[0]
	// Truncated output means unparseable JSON; fail here rather than letting
	// json.Unmarshal surface a confusing syntax error downstream.
	if choice.FinishReason != nil && *choice.FinishReason == components.ChatFinishReasonEnumLength {
		return "", errors.New("model output truncated (finish_reason=length)")
	}
	text, err := assistantText(choice.Message)
	if err != nil {
		if choice.FinishReason != nil {
			return "", fmt.Errorf("%w (finish_reason=%s)", err, *choice.FinishReason)
		}
		return "", err
	}
	return stripJSONFences(text), nil
}

// assistantText flattens the assistant message's content union to plain text.
// A set refusal wins over content: it means the model declined the request.
func assistantText(msg components.ChatAssistantMessage) (string, error) {
	if refusal, ok := msg.Refusal.Get(); ok && refusal != nil && *refusal != "" {
		return "", fmt.Errorf("model refused: %s", *refusal)
	}
	content, ok := msg.Content.Get()
	if !ok || content == nil {
		return "", errors.New("assistant message has no content")
	}
	switch content.Type {
	case components.ChatAssistantMessageContentTypeStr:
		if content.Str == nil {
			return "", errors.New("assistant content marked string but is nil")
		}
		return *content.Str, nil
	case components.ChatAssistantMessageContentTypeArrayOfChatContentItems:
		// Multimodal replies arrive as parts; the concatenated text parts
		// are the message. Non-text parts (images, audio) carry nothing
		// useful for us.
		var b strings.Builder
		for _, item := range content.ArrayOfChatContentItems {
			if item.ChatContentText != nil {
				b.WriteString(item.ChatContentText.Text)
			}
		}
		if b.Len() == 0 {
			return "", errors.New("assistant content items contain no text")
		}
		return b.String(), nil
	case components.ChatAssistantMessageContentTypeAny:
		// The SDK's catch-all variant. A string passes through; anything
		// else is re-marshalled so the caller still gets JSON to parse.
		if s, ok := content.Any.(string); ok {
			return s, nil
		}
		raw, err := json.Marshal(content.Any)
		if err != nil {
			return "", fmt.Errorf("re-marshal untyped assistant content: %w", err)
		}
		return string(raw), nil
	default:
		return "", fmt.Errorf("unhandled assistant content type %q", content.Type)
	}
}

// stripJSONFences unwraps a markdown code fence ("```json\n{...}\n```") if
// the model added one despite the prompt telling it not to.
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

const systemPrompt = `
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
