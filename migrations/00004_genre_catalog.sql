-- +goose Up

-- The genre catalog, managed by admins. It replaces the fixed movie_genre enum of 00003.
CREATE TABLE genres (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug       text NOT NULL CHECK (slug ~ '^[a-z0-9]+(_[a-z0-9]+)*$' AND length(slug) <= 50),
    name       text NOT NULL CHECK (name <> ''),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX genres_slug_uq ON genres (slug);
CREATE UNIQUE INDEX genres_name_uq ON genres (lower(name));

-- A movie's genres, most characteristic first (position 1). At most 5 per movie.
CREATE TABLE movie_genres (
    movie_id bigint   NOT NULL REFERENCES movies (id) ON DELETE CASCADE,
    genre_id bigint   NOT NULL REFERENCES genres (id), -- no cascade: a genre in use cannot be deleted
    position smallint NOT NULL CHECK (position BETWEEN 1 AND 5),
    PRIMARY KEY (movie_id, genre_id),
    UNIQUE (movie_id, position)
);
CREATE INDEX movie_genres_genre_idx ON movie_genres (genre_id);

-- The former vocabulary becomes the initial catalog, and every movie keeps its genres in order.
INSERT INTO genres (slug, name) VALUES
    ('action', 'Action'), ('adventure', 'Adventure'), ('animation', 'Animation'), ('comedy', 'Comedy'),
    ('crime', 'Crime'), ('documentary', 'Documentary'), ('drama', 'Drama'), ('family', 'Family'),
    ('fantasy', 'Fantasy'), ('history', 'History'), ('horror', 'Horror'), ('music', 'Music'),
    ('mystery', 'Mystery'), ('romance', 'Romance'), ('science_fiction', 'Science fiction'),
    ('thriller', 'Thriller'), ('war', 'War'), ('western', 'Western');

INSERT INTO movie_genres (movie_id, genre_id, position)
SELECT m.id, g.id, u.position
FROM movies m
CROSS JOIN LATERAL unnest(m.genres) WITH ORDINALITY AS u (genre, position)
JOIN genres g ON g.slug = u.genre::text;

ALTER TABLE movies DROP COLUMN genres;
DROP TYPE movie_genre;

-- +goose Down

-- Only genres whose slug the old enum knows can move back; genres added by admins are lost.
CREATE TYPE movie_genre AS ENUM (
    'action', 'adventure', 'animation', 'comedy', 'crime', 'documentary', 'drama', 'family', 'fantasy',
    'history', 'horror', 'music', 'mystery', 'romance', 'science_fiction', 'thriller', 'war', 'western'
);
ALTER TABLE movies
    ADD COLUMN genres movie_genre[] NOT NULL DEFAULT '{}' CHECK (cardinality(genres) <= 5);

UPDATE movies m
SET genres = back.genres
FROM (
    SELECT mg.movie_id, array_agg(g.slug::movie_genre ORDER BY mg.position) AS genres
    FROM movie_genres mg
    JOIN genres g ON g.id = mg.genre_id
    WHERE g.slug = ANY (enum_range(NULL::movie_genre)::text[])
    GROUP BY mg.movie_id
) back
WHERE m.id = back.movie_id;

DROP TABLE movie_genres;
DROP TABLE genres;
