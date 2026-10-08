CREATE TABLE IF NOT EXISTS public.voters (
    nisn VARCHAR(10) PRIMARY KEY CHECK (nisn ~ '^[0-9]{10}$'),
    voted BOOLEAN NOT NULL DEFAULT FALSE,
    blocked BOOLEAN NOT NULL DEFAULT FALSE,
    blocked_at TIMESTAMPTZ,
    reason TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS public.candidates (
    id BIGSERIAL PRIMARY KEY,
    number INTEGER NOT NULL UNIQUE CHECK (number > 0),
    name TEXT NOT NULL,
    vision TEXT NOT NULL,
    photo_url TEXT NOT NULL DEFAULT ''
);

-- Untuk database yang sudah ada: tambahkan kolom foto kandidat
ALTER TABLE public.candidates ADD COLUMN IF NOT EXISTS photo_url TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS public.candidate_votes (
    candidate_id BIGINT PRIMARY KEY,
    votes BIGINT NOT NULL DEFAULT 0 CHECK (votes >= 0)
);

CREATE TABLE IF NOT EXISTS public.app_migrations (
    name TEXT PRIMARY KEY,
    completed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE public.voters ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.candidates ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.candidate_votes ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.app_migrations ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON TABLE public.voters, public.candidate_votes, public.app_migrations FROM PUBLIC, anon, authenticated;
REVOKE ALL ON TABLE public.candidates FROM PUBLIC, anon, authenticated;
GRANT SELECT ON TABLE public.candidates TO anon;
GRANT ALL ON TABLE public.voters, public.candidates, public.candidate_votes, public.app_migrations TO service_role;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO service_role;

DROP POLICY IF EXISTS candidates_public_read ON public.candidates;
CREATE POLICY candidates_public_read
    ON public.candidates
    FOR SELECT
    TO anon
    USING (TRUE);

CREATE OR REPLACE FUNCTION public.cast_vote(p_nisn TEXT, p_candidate_id BIGINT)
RETURNS BOOLEAN
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    voter_blocked BOOLEAN;
    voter_voted BOOLEAN;
BEGIN
    SELECT blocked, voted
    INTO voter_blocked, voter_voted
    FROM public.voters
    WHERE nisn = p_nisn
    FOR UPDATE;

    IF NOT FOUND OR voter_blocked OR voter_voted THEN
        RETURN FALSE;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM public.candidates WHERE id = p_candidate_id) THEN
        RAISE EXCEPTION 'candidate_not_found' USING ERRCODE = '22023';
    END IF;

    UPDATE public.voters SET voted = TRUE WHERE nisn = p_nisn;
    INSERT INTO public.candidate_votes (candidate_id, votes)
    VALUES (p_candidate_id, 1)
    ON CONFLICT (candidate_id)
    DO UPDATE SET votes = public.candidate_votes.votes + 1;

    RETURN TRUE;
END;
$$;

REVOKE ALL ON FUNCTION public.cast_vote(TEXT, BIGINT) FROM PUBLIC, anon, authenticated;
GRANT EXECUTE ON FUNCTION public.cast_vote(TEXT, BIGINT) TO service_role;

CREATE OR REPLACE FUNCTION public.admin_stats()
RETURNS JSONB
LANGUAGE SQL
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT jsonb_build_object(
        'results', COALESCE((
            SELECT jsonb_agg(
                jsonb_build_object(
                    'id', c.id,
                    'number', c.number,
                    'name', c.name,
                    'vision', c.vision,
                    'photo_url', c.photo_url,
                    'votes', COALESCE(cv.votes, 0)
                )
                ORDER BY c.number, c.id
            )
            FROM public.candidates c
            LEFT JOIN public.candidate_votes cv ON cv.candidate_id = c.id
        ), '[]'::jsonb),
        'total', (SELECT COUNT(*) FROM public.voters),
        'voted', (SELECT COUNT(*) FROM public.voters WHERE voted),
        'blocked', (SELECT COUNT(*) FROM public.voters WHERE blocked)
    );
$$;

REVOKE ALL ON FUNCTION public.admin_stats() FROM PUBLIC, anon, authenticated;
GRANT EXECUTE ON FUNCTION public.admin_stats() TO service_role;

CREATE OR REPLACE FUNCTION public.import_legacy_data(p_data JSONB)
RETURNS BOOLEAN
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    item RECORD;
    max_candidate_id BIGINT;
BEGIN
    INSERT INTO public.app_migrations (name)
    VALUES ('legacy_json_v1')
    ON CONFLICT (name) DO NOTHING;
    IF NOT FOUND THEN
        RETURN FALSE;
    END IF;

    IF EXISTS (SELECT 1 FROM public.voters) OR EXISTS (SELECT 1 FROM public.candidates) THEN
        RETURN FALSE;
    END IF;

    FOR item IN SELECT key, value FROM jsonb_each(COALESCE(p_data->'voters', '{}'::jsonb))
    LOOP
        INSERT INTO public.voters (nisn, voted, blocked, blocked_at, reason)
        VALUES (
            item.key,
            COALESCE((item.value->>'voted')::BOOLEAN, FALSE),
            COALESCE((item.value->>'blocked')::BOOLEAN, FALSE),
            NULLIF(item.value->>'blockedAt', '')::TIMESTAMPTZ,
            COALESCE(item.value->>'reason', '')
        );
    END LOOP;

    INSERT INTO public.candidates (id, number, name, vision)
    SELECT
        (item->>'id')::BIGINT,
        (item->>'number')::INTEGER,
        item->>'name',
        COALESCE(item->>'vision', '')
    FROM jsonb_array_elements(COALESCE(p_data->'candidates', '[]'::jsonb)) AS candidates(item);

    FOR item IN SELECT key, value FROM jsonb_each(COALESCE(p_data->'counts', '{}'::jsonb))
    LOOP
        INSERT INTO public.candidate_votes (candidate_id, votes)
        VALUES (item.key::BIGINT, item.value::BIGINT)
        ON CONFLICT (candidate_id) DO UPDATE SET votes = EXCLUDED.votes;
    END LOOP;

    SELECT MAX(id) INTO max_candidate_id FROM public.candidates;
    IF max_candidate_id IS NOT NULL THEN
        PERFORM setval(pg_get_serial_sequence('public.candidates', 'id'), max_candidate_id, TRUE);
    END IF;

    RETURN TRUE;
END;
$$;

REVOKE ALL ON FUNCTION public.import_legacy_data(JSONB) FROM PUBLIC, anon, authenticated;
GRANT EXECUTE ON FUNCTION public.import_legacy_data(JSONB) TO service_role;
