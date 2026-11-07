-- name: LockCustomerIdentity :exec
SELECT pg_advisory_xact_lock(hashtextextended('customer-identity:' || sqlc.arg(kratos_identity_id)::uuid::text, 0));

-- name: GetCustomerProfileID :one
SELECT profile_id FROM customer_identity_links WHERE kratos_identity_id=$1;

-- name: LinkCustomerProfile :exec
INSERT INTO customer_identity_links(kratos_identity_id, profile_id) VALUES($1,$2);
