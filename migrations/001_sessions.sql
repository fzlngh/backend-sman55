-- Jalankan file ini sekali di Supabase SQL Editor (aman diulang).
-- ============================================================
-- Sesi login (pemilih & admin) disimpan di Supabase, bukan di memori server.
-- Wajib untuk deployment serverless (Vercel) yang memakai banyak instance.
-- Yang disimpan hanya hash SHA-256 dari token, bukan token mentah.
-- ============================================================
CREATE TABLE IF NOT EXISTS public.sessions (
    token_hash TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('voter', 'admin')),
    nisn TEXT,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((kind = 'voter') = (nisn IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS sessions_expires_at_idx ON public.sessions (expires_at);

ALTER TABLE public.sessions ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON TABLE public.sessions FROM PUBLIC, anon, authenticated;
GRANT ALL ON TABLE public.sessions TO service_role;
