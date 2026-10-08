-- The restore drill's fingerprint of one database (scripts/dev-restore-drill.sh,
-- STD-GLB-002 §Restore Evidence). One JSON object on one line:
--
--   migration   the newest version in Atlas's revision table
--   tables      every table, partitioned parents included: its row count, and an md5 over the md5 of
--               each row's text, sorted, so the checksum does not depend on physical order
--   sequences   every sequence's last value
--   roles       every role outside pg_*: its attributes and the roles it belongs to. Roles are the
--               cluster's, not the database's, and a dump of the database carries none of them
--
-- Read-only, and it creates nothing: a function would itself change the schema being compared, so
-- each table is counted and hashed through query_to_xml. Run as the superuser, which no Row-Level
-- Security policy filters, so every row is counted. The schema itself, owners and grants included,
-- is compared as pg_dump --schema-only.
SELECT json_build_object(
    'migration', (SELECT max(version) FROM atlas.atlas_schema_revisions),
    'tables', (
        SELECT coalesce(json_agg(json_build_object('table', t.name, 'rows', t.rows, 'checksum', t.checksum)
                                 ORDER BY t.name), '[]'::json)
          FROM (SELECT format('%I.%I', n.nspname, c.relname) AS name,
                       (xpath('/row/n/text()', query_to_xml(
                           format('SELECT count(*) AS n FROM %I.%I', n.nspname, c.relname),
                           false, true, '')))[1]::text::bigint AS rows,
                       (xpath('/row/h/text()', query_to_xml(
                           format('SELECT md5(coalesce(string_agg(md5(r::text), '''' ORDER BY md5(r::text)), '''')) AS h FROM %I.%I AS r',
                                  n.nspname, c.relname),
                           false, true, '')))[1]::text AS checksum
                  FROM pg_class c
                  JOIN pg_namespace n ON n.oid = c.relnamespace
                 WHERE c.relkind IN ('r', 'p')
                   AND n.nspname NOT IN ('pg_catalog', 'information_schema')
                   AND n.nspname NOT LIKE 'pg\_toast%'
                   AND n.nspname NOT LIKE 'pg\_temp%') AS t),
    'sequences', (
        SELECT coalesce(json_agg(json_build_object('sequence', format('%I.%I', schemaname, sequencename),
                                                   'last_value', last_value)
                                 ORDER BY schemaname, sequencename), '[]'::json)
          FROM pg_sequences),
    'roles', (
        SELECT coalesce(json_agg(json_build_object(
                   'role', r.rolname, 'superuser', r.rolsuper, 'inherit', r.rolinherit,
                   'createrole', r.rolcreaterole, 'createdb', r.rolcreatedb, 'login', r.rolcanlogin,
                   'replication', r.rolreplication, 'bypassrls', r.rolbypassrls,
                   'member_of', (SELECT coalesce(json_agg(g.rolname ORDER BY g.rolname), '[]'::json)
                                   FROM pg_auth_members m
                                   JOIN pg_roles g ON g.oid = m.roleid
                                  WHERE m.member = r.oid))
                 ORDER BY r.rolname), '[]'::json)
          FROM pg_roles r
         WHERE r.rolname NOT LIKE 'pg\_%')
);
