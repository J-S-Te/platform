ALTER TABLE iam_personnel_change_request
  ADD COLUMN handover_reference VARCHAR(128) NULL AFTER approval_reference,
  ADD COLUMN rejection_reason VARCHAR(500) NULL AFTER handover_reference;
