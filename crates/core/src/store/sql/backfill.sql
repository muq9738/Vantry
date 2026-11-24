INSERT OR IGNORE INTO inputs (
    id, thread_id, run_id, attempt_id, kind, message, attachments, command_id,
    status, disposition, error, created_at, updated_at
)
SELECT q.id, q.thread_id, q.run_id,
       COALESCE((SELECT r.active_attempt_id FROM runs r WHERE r.id = q.run_id), ''),
       'queue', q.message, q.attachments,
       COALESCE((SELECT c.command_id FROM command_receipts c
                 WHERE c.message_id = q.id OR c.input_id = q.id LIMIT 1), ''),
       CASE q.status WHEN 'pending' THEN 'queued' ELSE q.status END,
       '', '', q.created_at, q.created_at
FROM queued_messages q;

INSERT OR IGNORE INTO inputs (
    id, thread_id, run_id, attempt_id, kind, message, attachments, command_id,
    status, disposition, error, created_at, updated_at
)
SELECT s.id, s.thread_id, s.run_id, s.attempt_id, 'steer', s.message, s.attachments,
       s.command_id,
       CASE s.status
           WHEN 'accepted' THEN 'delivered'
           WHEN 'rejected' THEN 'failed'
           WHEN 'sending' THEN 'unknown'
           WHEN 'pending' THEN 'failed'
           ELSE s.status END,
       s.disposition,
       CASE s.status
           WHEN 'pending' THEN 'steering was persisted before the send barrier and will not be retried'
           ELSE s.error END,
       s.created_at, s.updated_at
FROM steering_messages s;

WITH positions AS (
    SELECT pending.id, ROW_NUMBER() OVER (
        PARTITION BY pending.thread_id ORDER BY pending.created_at, pending.id
    ) - 1 + COALESCE((
        SELECT MAX(existing.queue_order) + 1 FROM inputs existing
        WHERE existing.thread_id = pending.thread_id AND existing.kind = 'queue'
          AND existing.status = 'queued' AND existing.queue_order >= 0
    ), 0) AS position
    FROM inputs pending
    WHERE pending.kind = 'queue' AND pending.status = 'queued' AND pending.queue_order < 0
)
UPDATE inputs SET queue_order = (SELECT position FROM positions WHERE positions.id = inputs.id)
WHERE id IN (SELECT id FROM positions);

UPDATE inputs SET queue_order = 0 WHERE queue_order < 0;
