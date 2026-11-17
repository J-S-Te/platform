-- 字典项语义元数据（2026-11-17）
-- 状态类字典需要把“语义色调”下发给业务前端；色调只表达语义（neutral/processing/success/
-- warning/danger），具体色值始终由前端设计 token 决定，因此这里不存任何 hex/rgb。
-- 新增列全部可空或带默认值，旧数据与未接入的服务保持原行为。
ALTER TABLE dict_item
    ADD COLUMN i18n_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER label,
    ADD COLUMN tone VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER item_value,
    ADD COLUMN terminal TINYINT(1) NOT NULL DEFAULT 0 AFTER sort_order,
    ADD COLUMN actionable TINYINT(1) NOT NULL DEFAULT 1 AFTER terminal;
