// Command backchannel-inventory 只读盘点"有活跃登录状态但缺少 Back-Channel Logout
// 登记"的 OAuth 客户端（BIZ-XEVT2-07 的存量回填入口）。
//
// 它是只读工具：不写登记、不改状态、不部署；运维确认影响面后再走受控的幂等补登记
// （接入流程或受控更新路径）。输出只含租户、客户端标识与活跃 token family 数，
// 不含任何密钥、令牌、会话内容或完整运行配置。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"text/tabwriter"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/backchannel"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/database"
)

func main() {
	if err := run(); err != nil {
		slog.Error("back-channel logout inventory stopped with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	asJSON := flag.Bool("json", false, "以 JSON 输出（便于工单与自动化比对），默认输出表格")
	timeout := flag.Duration("timeout", 30*time.Second, "只读盘点的超时时间")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := database.OpenMySQL(cfg.MySQL)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close(db) }()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	clients, err := backchannel.UnregisteredClients(ctx, db)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeInventoryJSON(os.Stdout, clients)
	}
	return writeInventoryTable(os.Stdout, clients)
}

// inventoryRow 是可安全展示的盘点行：只含租户、客户端标识与活跃 family 数。
// 刻意不包含 secret、token、会话 ID 或回调内容，避免盘点输出变成凭据泄露渠道。
type inventoryRow struct {
	TenantID       string `json:"tenant_id"`
	ClientID       string `json:"client_id"`
	OAuthClientID  string `json:"oauth_client_id"`
	ActiveFamilies int64  `json:"active_families"`
}

// inventoryRows 把只读盘点结果投影为输出行；顺序沿用 SQL 的稳定排序。
func inventoryRows(clients []backchannel.UnregisteredClient) []inventoryRow {
	rows := make([]inventoryRow, 0, len(clients))
	for _, client := range clients {
		rows = append(rows, inventoryRow{
			TenantID:       client.TenantID,
			ClientID:       client.ClientID,
			OAuthClientID:  client.OAuthClientID,
			ActiveFamilies: client.ActiveFamilies,
		})
	}
	return rows
}

// writeInventoryTable 以表格输出盘点结果；没有缺口时明确打印"无"而不是空白，
// 避免把"查询成功且没有缺口"误读成"工具没跑"。
func writeInventoryTable(writer io.Writer, clients []backchannel.UnregisteredClient) error {
	rows := inventoryRows(clients)
	if len(rows) == 0 {
		_, err := fmt.Fprintln(writer, "没有缺少 Back-Channel Logout 登记的活跃客户端")
		return err
	}
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "TENANT\tCLIENT_ID\tOAUTH_CLIENT_ID\tACTIVE_FAMILIES"); err != nil {
		return err
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%d\n", row.TenantID, row.ClientID, row.OAuthClientID, row.ActiveFamilies); err != nil {
			return err
		}
	}
	return table.Flush()
}

// writeInventoryJSON 输出同一份投影的 JSON 形式，字段固定，便于工单与自动化比对。
func writeInventoryJSON(writer io.Writer, clients []backchannel.UnregisteredClient) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(inventoryRows(clients))
}
