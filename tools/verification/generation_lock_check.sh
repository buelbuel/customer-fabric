#!/bin/bash
# ADR 0010 rule 3: which locks conflict with the generation row's FOR KEY SHARE, on real PostgreSQL.
# Requires Docker. Starts a throwaway container with synthetic data and removes it on exit.
set -u
N=cf-lockcheck-$$
docker run -d --rm --name $N -e POSTGRES_PASSWORD=synthetic postgres:17-alpine >/dev/null
trap 'docker rm -f $N >/dev/null' EXIT
for i in $(seq 1 40); do docker exec $N pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1 && break; sleep 1; done
sleep 1
P="docker exec -i $N psql -h 127.0.0.1 -U postgres -qAt"
docker exec $N psql -h 127.0.0.1 -U postgres -qAtc "select version(); create table gen(id int primary key, g int); insert into gen values (1,0);"
try(){ # label, blocker sql, contender sql
  (printf "begin; select 1 from gen where id=1 FOR KEY SHARE; select pg_sleep(3); commit;\n" | $P >/dev/null) &
  sleep 1
  OUT=$(printf "set lock_timeout='500ms'; begin; $2; commit;\n" | $P 2>&1 | tail -1)
  echo "$1 => ${OUT:-acquired}"
  wait
}
try "UPDATE (non-key) against FOR KEY SHARE" "update gen set g=g+1 where id=1"
try "FOR NO KEY UPDATE against FOR KEY SHARE" "select 1 from gen where id=1 for no key update"
try "FOR UPDATE against FOR KEY SHARE" "select 1 from gen where id=1 for update"
# A session that outlives a pg_hba/network change is removed only by termination.
(printf "begin; select 1 from gen where id=1 FOR KEY SHARE; select pg_sleep(20); commit;\n" | $P >/dev/null 2>&1) &
sleep 1
docker exec $N psql -h 127.0.0.1 -U postgres -qAtc "select 'terminated '||count(pg_terminate_backend(pid)) from pg_stat_activity where pid<>pg_backend_pid() and backend_type='client backend'"
sleep 1
printf "set lock_timeout='500ms'; begin; select 'FOR UPDATE after termination acquired' from gen where id=1 for update; commit;\n" | $P 2>&1 | tail -1
wait
