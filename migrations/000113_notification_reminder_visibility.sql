-- 提醒频率（IMMEDIATE/DAILY/WEEKLY/NEVER）真实生效：
--   IMMEDIATE  创建即可见（原默认行为）
--   DAILY      投递写入但延迟到下一个每日释放点（北京时间 09:00）可见
--   WEEKLY     延迟到下一个每周释放点（北京时间周一 09:00）可见
--   NEVER      不进入站内信（创建被抑制）
-- 旧词表 EVERY_FOUR_HOURS/ONCE 没有任何投递语义且前端从未使用，统一归一化为 IMMEDIATE，
-- 保证存量设置的行为与迁移前完全一致（旧实现中频率不产生任何效果）。
-- notification_user_stat 不再作为未读口径：延迟可见使增量计数失效，未读数改为按
-- delivery 的 remind_at 可见性过滤直接计数；本表保留但平台代码停止读写。
ALTER TABLE notification_delivery
    ADD COLUMN remind_at DATETIME(3) NULL,
    ADD KEY idx_notification_delivery_remind (tenant_id, recipient_user_id, remind_at);

UPDATE notification_setting
SET reminder_frequency = 'IMMEDIATE'
WHERE reminder_frequency NOT IN ('IMMEDIATE', 'DAILY', 'WEEKLY', 'NEVER');
