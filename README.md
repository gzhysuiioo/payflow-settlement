# 链上支付与结算编排平台

## 用途

账户抽象、付款方与代付策略、批量结算、幂等与对账、失败补偿、手续费与限额治理、可审计流水。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `payflow/`，命令入口位于 `cmd/payflow/`。

```bash
go run ./cmd/payflow demo
go run ./cmd/payflow version
go test ./...
```

## 技术方向

wallet, paymaster, eip4337, account-abstraction, payment-channel, bundler, custody

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
