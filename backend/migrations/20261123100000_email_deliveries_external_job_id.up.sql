ALTER TABLE email_deliveries
    ADD COLUMN IF NOT EXISTS external_job_id TEXT;

