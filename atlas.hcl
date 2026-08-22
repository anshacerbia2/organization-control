// Atlas project configuration, per ADR-GLB-004.
//
// Every environment scopes Atlas to the seven schemas this service owns. That scope is
// load-bearing rather than tidy: the same database also carries `platform`, which
// foundation-platform owns and ships as versioned SQL. Unscoped, Atlas reads `platform` as
// drift against schema.hcl and plans to drop it — which is what a database-scoped plan did
// the first time identity-control generated one, ending in `DROP SCHEMA "public" CASCADE`.

variable "url" {
  type    = string
  default = getenv("DATABASE_URL")
}

// Atlas computes a diff by materialising the desired state on a throwaway database. It must
// be a real PostgreSQL of the same major version the schema is deployed against — a version
// mismatch produces a plan that is correct for the wrong server.
variable "dev_url" {
  type    = string
  default = getenv("ATLAS_DEV_URL")
}

// The schemas Atlas owns. `platform` is absent deliberately; see schema.hcl.
//
// Row-Level Security is absent too, and for a different reason: Atlas OSS does not model
// `ENABLE`/`FORCE ROW LEVEL SECURITY` or `CREATE POLICY`, so those cannot live in
// schema.hcl. `internal/controldb/rls.sql` applies them in a stage after Atlas, and the
// structural test in that package is what keeps them from silently disappearing — because
// nothing reconciles them the way Atlas reconciles a column.
locals {
  // `public` is managed and declared empty in schema.hcl. Atlas requires every schema the HCL
  // names to appear here, and a multi-schema source works in database scope, so a schema that
  // exists in the database and is absent from the source reads as drift — the first plan
  // generated here ended in `DROP SCHEMA "public" CASCADE`. Managing it empty is both true and
  // useful: a table appearing in `public` is drift, and Atlas now reports it.
  managed_schemas = ["public", "organization", "tenant", "workspace", "membership", "invitation", "operation", "projection"]

  // Everything Atlas must leave alone, stated rather than inferred.
  //
  // `schemas` alone does not bound a multi-schema HCL diff. With seven schemas declared Atlas
  // works in database scope, and the first plan generated without this list ended in
  // `DROP SCHEMA "public" CASCADE` -- the same failure identity-control hit, where one
  // `search_path` was enough to prevent it. `platform` would have followed for the same reason:
  // it is real, it is undeclared here, and an undeclared object in scope reads as drift.
  //
  // `atlas` is the revision store. Excluding it keeps Atlas from planning against its own
  // bookkeeping.
  foreign_schemas = ["platform", "platform.*", "atlas", "atlas.*"]
}

env "local" {
  src     = "file://schema.hcl"
  url     = var.url
  dev     = var.dev_url
  schemas = local.managed_schemas
  exclude = local.foreign_schemas

  migration {
    dir = "file://migrations"

    // Atlas's own bookkeeping lives outside the schemas it manages.
    //
    // Without this it lands in one of them, and grants.sql grants the runtime roles DML on
    // every table in every owned schema — so a runtime role could rewrite migration history,
    // and a later `atlas migrate apply` would re-run or skip migrations based on rows the
    // application was able to change. Found in identity-control by querying
    // has_table_privilege rather than by reading the configuration.
    revisions_schema = "atlas"
  }

  format {
    migrate {
      diff = "{{ sql . \"  \" }}"
    }
  }
}

env "ci" {
  src     = "file://schema.hcl"
  url     = var.url
  dev     = var.dev_url
  schemas = local.managed_schemas
  exclude = local.foreign_schemas

  migration {
    dir              = "file://migrations"
    revisions_schema = "atlas"
  }

  // ADR-GLB-004 requires the pipeline to block a destructive plan rather than report it, and
  // names `atlas migrate lint` as the mechanism. Since Atlas v0.38 that command aborts on the
  // free CLI: "'atlas migrate lint' is available only to Atlas Pro users."
  //
  // This block is therefore inert today. It is kept rather than deleted because it is correct
  // the moment an Atlas Pro login exists, and deleting it would erase the record that the
  // mandated configuration was written. ci.yml runs `atlas migrate validate`, which is free
  // and checks directory integrity, plus a text-level destructive gate standing in for the
  // analyzer. That substitution is recorded as debt in ROADMAP.md.
  lint {
    destructive {
      error = true
    }
    incompatible {
      error = true
    }
  }
}
