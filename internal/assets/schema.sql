CREATE TABLE IF NOT EXISTS projects (
  id VARCHAR(32) PRIMARY KEY,
  name VARCHAR(255) NOT NULL,
  root_path VARCHAR(2048) NOT NULL,
  base_branch VARCHAR(255) NOT NULL,
  config_json LONGTEXT NOT NULL,
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL
);

CREATE TABLE IF NOT EXISTS collections (
  id VARCHAR(32) PRIMARY KEY,
  project_id VARCHAR(32) NOT NULL,
  title VARCHAR(512) NOT NULL,
  objective LONGTEXT NOT NULL,
  status VARCHAR(32) NOT NULL,
  integration_branch VARCHAR(512),
  integration_worktree VARCHAR(2048),
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  INDEX idx_collections_project_status (project_id, status)
);

CREATE TABLE IF NOT EXISTS tasks (
  id VARCHAR(32) PRIMARY KEY,
  project_id VARCHAR(32) NOT NULL,
  collection_id VARCHAR(32) NOT NULL,
  parent_task_id VARCHAR(32),
  requested_by_task_id VARCHAR(32),
  profile VARCHAR(32) NOT NULL,
  title VARCHAR(512) NOT NULL,
  objective LONGTEXT NOT NULL,
  reason LONGTEXT,
  status VARCHAR(32) NOT NULL,
  priority INT NOT NULL DEFAULT 50,
  depth INT NOT NULL DEFAULT 0,
  workspace_mode VARCHAR(32) NOT NULL DEFAULT 'collection',
  base_commit VARCHAR(128),
  branch_name VARCHAR(512),
  worktree_path VARCHAR(2048),
  current_head VARCHAR(128),
  dedupe_key VARCHAR(64),
  result_json LONGTEXT,
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  INDEX idx_tasks_collection_status (collection_id, status),
  INDEX idx_tasks_project_status (project_id, status),
  INDEX idx_tasks_dedupe (collection_id, dedupe_key)
);

CREATE TABLE IF NOT EXISTS task_edges (
  from_task_id VARCHAR(32) NOT NULL,
  to_task_id VARCHAR(32) NOT NULL,
  collection_id VARCHAR(32) NOT NULL,
  relation VARCHAR(32) NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (from_task_id, to_task_id, relation),
  INDEX idx_edges_to (to_task_id, relation),
  INDEX idx_edges_from (from_task_id, relation)
);

CREATE TABLE IF NOT EXISTS attempts (
  id VARCHAR(32) PRIMARY KEY,
  task_id VARCHAR(32) NOT NULL,
  attempt_no INT NOT NULL,
  status VARCHAR(32) NOT NULL,
  exit_code INT,
  external_session_id VARCHAR(255),
  log_path VARCHAR(2048),
  started_at DATETIME(6),
  ended_at DATETIME(6),
  result_json LONGTEXT,
  created_at DATETIME(6) NOT NULL,
  UNIQUE KEY uq_attempt_no (task_id, attempt_no),
  INDEX idx_attempt_task_status (task_id, status)
);

CREATE TABLE IF NOT EXISTS sessions (
  id VARCHAR(32) PRIMARY KEY,
  attempt_id VARCHAR(32) NOT NULL,
  harness VARCHAR(64) NOT NULL,
  external_session_id VARCHAR(255),
  pid INT,
  status VARCHAR(32) NOT NULL,
  workdir VARCHAR(2048),
  started_at DATETIME(6),
  ended_at DATETIME(6),
  created_at DATETIME(6) NOT NULL,
  INDEX idx_sessions_attempt (attempt_id)
);

CREATE TABLE IF NOT EXISTS events (
  id VARCHAR(32) PRIMARY KEY,
  project_id VARCHAR(32),
  collection_id VARCHAR(32),
  task_id VARCHAR(32),
  attempt_id VARCHAR(32),
  event_type VARCHAR(128) NOT NULL,
  payload_json LONGTEXT,
  created_at DATETIME(6) NOT NULL,
  INDEX idx_events_collection (collection_id, created_at),
  INDEX idx_events_task (task_id, created_at)
);

CREATE TABLE IF NOT EXISTS memories (
  id VARCHAR(32) PRIMARY KEY,
  project_id VARCHAR(32) NOT NULL,
  collection_id VARCHAR(32),
  task_id VARCHAR(32),
  scope VARCHAR(32) NOT NULL,
  state VARCHAR(32) NOT NULL,
  kind VARCHAR(64) NOT NULL,
  text LONGTEXT NOT NULL,
  evidence_json LONGTEXT,
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  INDEX idx_memories_project_scope (project_id, scope, state)
);

CREATE TABLE IF NOT EXISTS decisions (
  id VARCHAR(32) PRIMARY KEY,
  project_id VARCHAR(32) NOT NULL,
  collection_id VARCHAR(32),
  task_id VARCHAR(32),
  statement LONGTEXT NOT NULL,
  rationale LONGTEXT,
  evidence_json LONGTEXT,
  alternatives_json LONGTEXT,
  superseded_by VARCHAR(32),
  created_at DATETIME(6) NOT NULL,
  INDEX idx_decisions_project (project_id, created_at)
);



CREATE TABLE IF NOT EXISTS collection_revisions (
  id VARCHAR(32) PRIMARY KEY,
  project_id VARCHAR(32) NOT NULL,
  collection_id VARCHAR(32) NOT NULL,
  reason LONGTEXT NOT NULL,
  status VARCHAR(32) NOT NULL,
  coordinator_task_id VARCHAR(32),
  created_at DATETIME(6) NOT NULL,
  satisfied_at DATETIME(6),
  INDEX idx_collection_revisions_collection (collection_id, status, created_at)
);

CREATE TABLE IF NOT EXISTS collection_acceptances (
  id VARCHAR(32) PRIMARY KEY,
  project_id VARCHAR(32) NOT NULL,
  collection_id VARCHAR(32) NOT NULL,
  target_branch VARCHAR(512) NOT NULL,
  source_branch VARCHAR(512) NOT NULL,
  source_commit VARCHAR(128) NOT NULL,
  target_before_commit VARCHAR(128) NOT NULL,
  accepted_commit VARCHAR(128) NOT NULL,
  merge_mode VARCHAR(32) NOT NULL,
  actor VARCHAR(255),
  created_at DATETIME(6) NOT NULL,
  UNIQUE KEY uq_collection_accept_target (collection_id, target_branch),
  INDEX idx_acceptances_project (project_id, created_at),
  INDEX idx_acceptances_collection (collection_id, created_at)
);

CREATE TABLE IF NOT EXISTS collection_pull_requests (
  id VARCHAR(32) PRIMARY KEY,
  project_id VARCHAR(32) NOT NULL,
  collection_id VARCHAR(32) NOT NULL,
  provider VARCHAR(32) NOT NULL,
  remote_name VARCHAR(255) NOT NULL,
  repository VARCHAR(1024),
  pr_number INT NOT NULL,
  url VARCHAR(2048) NOT NULL,
  state VARCHAR(32) NOT NULL,
  is_draft BOOLEAN NOT NULL DEFAULT 0,
  base_branch VARCHAR(512) NOT NULL,
  head_branch VARCHAR(512) NOT NULL,
  source_commit VARCHAR(128) NOT NULL,
  title VARCHAR(1024) NOT NULL,
  actor VARCHAR(255),
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  UNIQUE KEY uq_collection_pr_provider (collection_id, provider),
  INDEX idx_collection_prs_project (project_id, created_at),
  INDEX idx_collection_prs_collection (collection_id, created_at)
);
