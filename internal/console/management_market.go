package console

import (
	"context"
	"torouter/internal/management"
)

type marketListInput struct {
	Query string `json:"query,omitempty"`
}
type marketRefreshInput struct {
	command
	Query string `json:"query,omitempty"`
}
type marketAuthorizationInput struct {
	command
	PackageID string `json:"package_id"`
	Provider  string `json:"provider"`
	marketStartInput
}
type marketFlowInput struct {
	command
	FlowID string  `json:"flow_id"`
	Code   *string `json:"code,omitempty"`
}

func (t *managementTools) marketOperations() []management.Operation {
	return []management.Operation{
		typedOperation[marketListInput]("list_provider_market", "plugins.market.list", management.Read, "查找供应商市场、安装版本、登录方法和必要输入。Magpie社区角标只表示维护来源；安装可执行代码需在本机插件库操作。", true, func(ctx context.Context, p management.Principal, in marketListInput) (any, error) {
			return t.services.plugins.MarketCatalogue(ctx, in.Query)
		}),
		typedOperation[marketRefreshInput]("refresh_provider_market", "plugins.market.refresh", management.Manage, "刷新公开供应商目录和版本；不安装或更新任何可执行代码。", false, func(ctx context.Context, p management.Principal, in marketRefreshInput) (any, error) {
			if err := t.services.plugins.RefreshMarket(ctx, in.Query); err != nil {
				return nil, err
			}
			return t.services.plugins.MarketCatalogue(ctx, in.Query)
		}),
		typedOperation[marketAuthorizationInput]("start_provider_authorization", "plugins.market.authorize", management.Manage, "为已安装并启用的市场供应商开始独立授权。先查list_provider_market取得package_id、provider、method与inputs字段；用户在供应商页面完成登录，API方式仅填写本次明确提供的密钥。", false, func(ctx context.Context, p management.Principal, in marketAuthorizationInput) (any, error) {
			v, err := t.oauth.startMarketCore(t.flowContext(ctx, p), in.PackageID, in.Provider, in.marketStartInput)
			if err == nil {
				if id, ok := asObject(v)["flow_id"].(string); ok {
					t.flowOwners.Store(id, p.ID)
				}
			}
			return v, err
		}),
		typedOperation[marketFlowInput]("get_provider_authorization", "plugins.market.status", management.Manage, "查询本连接发起的供应商授权。需要粘贴验证码时传code；成功后保存凭证并返回channel_id，不返回令牌。", false, func(ctx context.Context, p management.Principal, in marketFlowInput) (any, error) {
			return t.oauth.statusMarketCore(t.flowContext(ctx, p), in.FlowID, in.Code, false)
		}),
		typedOperation[marketFlowInput]("cancel_provider_authorization", "plugins.market.cancel", management.Manage, "取消本连接尚未完成的市场供应商授权；不删除已创建渠道。", false, func(ctx context.Context, p management.Principal, in marketFlowInput) (any, error) {
			return t.oauth.statusMarketCore(t.flowContext(ctx, p), in.FlowID, nil, true)
		}),
	}
}
