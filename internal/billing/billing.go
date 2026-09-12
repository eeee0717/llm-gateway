// Package billing 按用量计费：转发前预扣最大可能的费用，请求结束后按实际用量结算、多退少补，
// 并留下一条用量记录。规则见 docs/adr/0001，余额只存在 PostgreSQL 见 docs/adr/0002。
package billing

import (
	"context"
	"errors"
	"fmt"
	"math"

	"gorm.io/gorm"

	"github.com/eeee0717/llm-gateway/internal/apikey"
	"github.com/eeee0717/llm-gateway/internal/openai"
)

// perMillion 是单价的分母：Price 记的是一百万个 token 的价钱。
const perMillion = 1_000_000

// Price 是一个模型的单价，单位是微元每百万 token，由 PricePerMillionTokens 从配置里的价钱换算而来。
// 存成整数是为了让费用全程整数运算：单价常有小数（0.14 元每百万 token），
// 直接拿 float64 相乘会得到 7.000000000000001 这样的结果，向上取整就多收了 1 微元。
type Price struct {
	Input  int64
	Output int64
}

// PricePerMillionTokens 把配置里的单价（元每百万 token）换算成 Price。
// 1 元 = 1e6 微元，所以换算就是乘以 1e6；小数点后 6 位以下的价钱四舍五入，那已经不到 1 微元。
func PricePerMillionTokens(input, output float64) Price {
	return Price{
		Input:  int64(math.Round(input * perMillion)),
		Output: int64(math.Round(output * perMillion)),
	}
}

// Reservation 是一次预扣所需的信息：这次请求最多可能花多少钱，由它算出来。
type Reservation struct {
	Model           string
	PromptTokens    int // 估算的 prompt token
	MaxOutputTokens int // 这次请求的输出上限
}

// Result 是一次请求结束后要结算的内容。
type Result struct {
	RequestID     string
	Model         string
	Usage         openai.Usage
	Estimated     bool  // 用量是网关估算的，而不是上游报告的
	ReservedMicro int64 // 这次请求预扣的金额，结算时多退少补
}

// Service 按 context 里的 API Key 计费。
type Service struct {
	store  *store
	prices map[string]Price
}

func New(db *gorm.DB, prices map[string]Price) *Service {
	return &Service{store: newStore(db), prices: prices}
}

// Reserve 按输出上限算出最大可能的费用并预扣。
func (s *Service) Reserve(ctx context.Context, r Reservation) (int64, bool, error) {
	keyID, ok := apikey.From(ctx)
	if !ok {
		return 0, false, errors.New("no api key in context")
	}
	amount := s.cost(r.Model, openai.Usage{PromptTokens: r.PromptTokens, CompletionTokens: r.MaxOutputTokens})
	ok, err := s.store.reserve(ctx, keyID, amount)
	if err != nil {
		return 0, false, fmt.Errorf("reserve for api key %d: %w", keyID, err)
	}
	return amount, ok, nil
}

// Settle 按实际用量结算：预扣和实际费用的差额退回余额，同时写一条用量记录。
// 错误里带上 API Key 的 ID：结算失败时那笔预扣还挂在余额上，对账要找的就是这个 Key。
func (s *Service) Settle(ctx context.Context, r Result) error {
	keyID, ok := apikey.From(ctx)
	if !ok {
		return errors.New("no api key in context")
	}
	// 请求 ID 是结算的幂等键，空的话所有请求会撞在同一个主键上，只有第一个能结算，其余的预扣都退不回来
	if r.RequestID == "" {
		return fmt.Errorf("settle for api key %d: request id is empty", keyID)
	}
	err := s.store.settle(ctx, record{
		requestID:     r.RequestID,
		keyID:         keyID,
		model:         r.Model,
		usage:         r.Usage,
		costMicro:     s.cost(r.Model, r.Usage),
		reservedMicro: r.ReservedMicro,
		estimated:     r.Estimated,
	})
	if err != nil {
		return fmt.Errorf("settle for api key %d: %w", keyID, err)
	}
	return nil
}

// cost 按单价折算费用，向上取整到 1 微元。模型的单价在启动时校验过，这里一定查得到。
func (s *Service) cost(model string, u openai.Usage) int64 {
	p := s.prices[model]
	micro := int64(u.PromptTokens)*p.Input + int64(u.CompletionTokens)*p.Output
	return (micro + perMillion - 1) / perMillion // 向上取整
}
