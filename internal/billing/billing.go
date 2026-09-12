// Package billing 按用量计费：转发前预扣最大可能的费用，请求结束后按实际用量结算、多退少补，
// 并留下一条用量记录。规则见 docs/adr/0001，余额只存在 PostgreSQL 见 docs/adr/0002。
package billing

import (
	"context"
	"errors"
	"log/slog"
	"math"

	"gorm.io/gorm"

	"github.com/eeee0717/llm-gateway/internal/apikey"
	"github.com/eeee0717/llm-gateway/internal/openai"
	"github.com/eeee0717/llm-gateway/internal/relay"
)

// Price 是一个模型的单价，单位是元每百万 token。
// 1 元 = 1e6 微元，1 百万 token = 1e6 token，所以这个数字同时就是"微元每 token"，算费用时不用换算。
type Price struct {
	Input  float64
	Output float64
}

// Service 实现 relay.Biller：扣谁的钱由 context 里的 API Key 决定。
type Service struct {
	store  *store
	prices map[string]Price
	logger *slog.Logger
}

func New(logger *slog.Logger, db *gorm.DB, prices map[string]Price) *Service {
	return &Service{store: newStore(db), prices: prices, logger: logger}
}

// Reserve 按输出上限算出最大可能的费用并预扣。
func (s *Service) Reserve(ctx context.Context, r relay.Reservation) (int64, bool, error) {
	keyID, ok := apikey.From(ctx)
	if !ok {
		return 0, false, errors.New("no api key in context")
	}
	amount := s.cost(r.Model, openai.Usage{PromptTokens: r.PromptTokens, CompletionTokens: r.MaxOutputTokens})
	ok, err := s.store.reserve(ctx, keyID, amount)
	if err != nil {
		return 0, false, err
	}
	return amount, ok, nil
}

// Settle 按实际用量结算：预扣和实际费用的差额退回余额，同时写一条用量记录。
func (s *Service) Settle(ctx context.Context, r relay.Result) error {
	keyID, ok := apikey.From(ctx)
	if !ok {
		return errors.New("no api key in context")
	}
	return s.store.settle(ctx, record{
		requestID:     r.RequestID,
		keyID:         keyID,
		model:         r.Model,
		usage:         r.Usage,
		costMicro:     s.cost(r.Model, r.Usage),
		reservedMicro: r.ReservedMicro,
		estimated:     r.Estimated,
	})
}

// cost 按单价折算费用，向上取整到 1 微元。模型的单价在启动时校验过，这里一定查得到。
func (s *Service) cost(model string, u openai.Usage) int64 {
	p := s.prices[model]
	return int64(math.Ceil(float64(u.PromptTokens)*p.Input + float64(u.CompletionTokens)*p.Output))
}
