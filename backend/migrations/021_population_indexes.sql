-- +goose Up
-- Population evaluation reads carried consent decisions through every association a customer has ever
-- had, including revoked ones, which the partial active-association index cannot serve (#134 Delivery A).
CREATE INDEX verified_associations_customer ON verified_associations(customer_subject);

-- +goose Down
DROP INDEX verified_associations_customer;
