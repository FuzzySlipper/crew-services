-- A claim that found nothing to claim changes no delivery, so it is no longer
-- recorded as an operation receipt. Drop the ones earlier versions stored.
DELETE FROM adapter_operation_receipts
WHERE kind = 'claim' AND json_extract(result_json, '$.claim.claimed') = 0;

-- Retention prunes receipts by age.
CREATE INDEX adapter_operation_receipts_created_idx ON adapter_operation_receipts(created_at);
