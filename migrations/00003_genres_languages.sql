-- +goose Up

-- The genre vocabulary. It equals domain.Genres(); a new genre needs ALTER TYPE movie_genre ADD VALUE and the
-- domain list.
CREATE TYPE movie_genre AS ENUM (
    'action', 'adventure', 'animation', 'comedy', 'crime', 'documentary', 'drama', 'family', 'fantasy',
    'history', 'horror', 'music', 'mystery', 'romance', 'science_fiction', 'thriller', 'war', 'western'
);

-- A movie's genres, most characteristic first. Existing movies have none.
ALTER TABLE movies
    ADD COLUMN genres movie_genre[] NOT NULL DEFAULT '{}' CHECK (cardinality(genres) <= 5);

-- The language version of a showtime: the language of its soundtrack and of its subtitles, if any, as ISO 639-3
-- codes such as eng or tha. Existing showtimes were English; after the backfill, every new showtime must state
-- its audio.
ALTER TABLE showtimes
    ADD COLUMN audio_language    text NOT NULL DEFAULT 'eng' CHECK (audio_language ~ '^[a-z]{3}$'),
    ADD COLUMN subtitle_language text CHECK (subtitle_language ~ '^[a-z]{3}$');
ALTER TABLE showtimes ALTER COLUMN audio_language DROP DEFAULT;

-- +goose Down

ALTER TABLE showtimes DROP COLUMN subtitle_language, DROP COLUMN audio_language;
ALTER TABLE movies DROP COLUMN genres;
DROP TYPE movie_genre;
