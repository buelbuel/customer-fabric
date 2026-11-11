// Invoked only through the local Docker operator bootstrap command.
const roles = ["profile_reader", "governance_admin", "consent_admin", "audience_manager", "activation_operator",
  "identity_reviewer", "source_admin", "audit_reader", "application_admin", "privacy_admin"];
try {
  const input = await Bun.stdin.json();
  let path;
  let method;
  let body;
  if (input.action === "create") {
    path = "/admin/identities";
    method = "POST";
    body = JSON.stringify(input.identity);
  } else if (input.action === "disable" && /^[a-f0-9-]{36}$/.test(input.id)) {
    path = "/admin/identities/" + input.id;
    method = "PATCH";
    body = JSON.stringify([{ op: "replace", path: "/state", value: "inactive" }]);
  } else if (input.action === "roles" && /^[a-f0-9-]{36}$/.test(input.id) && Array.isArray(input.roles) &&
             input.roles.every(role => roles.includes(role))) {
    // Roles live in admin-managed metadata, which operators cannot change through self-service. The role
    // list replaces the previous one and any legacy single role; other metadata stays, and an unknown name
    // is refused rather than ignored.
    const current = await fetch("http://127.0.0.1:4434/admin/identities/" + input.id, { redirect: "error", signal: AbortSignal.timeout(10000) });
    if (!current.ok) throw new Error("Unknown operator");
    const metadata = { ...((await current.json()).metadata_public ?? {}), customer_fabric_roles: [...new Set(input.roles)] };
    delete metadata.customer_fabric_role;
    path = "/admin/identities/" + input.id;
    method = "PATCH";
    body = JSON.stringify([{ op: "replace", path: "/metadata_public", value: metadata }]);
  } else if (input.action === "revoke" && /^[a-f0-9-]{36}$/.test(input.id)) {
    path = "/admin/identities/" + input.id + "/sessions";
    method = "DELETE";
  } else {
    throw new Error("Unsupported action");
  }
  const result = await fetch("http://127.0.0.1:4434" + path, {
    method, body, headers: { "Content-Type": "application/json" },
    redirect: "error", signal: AbortSignal.timeout(10000),
  });
  if (!result.ok) {
    console.error("Operator administration returned HTTP " + result.status);
    process.exit(1);
  }
  if (input.action === "create") {
    const { id } = await result.json();
    if (!/^[a-f0-9-]{36}$/.test(id)) throw new Error("Invalid identity response");
    console.log(id);
  }
} catch {
  console.error("Operator administration failed");
  process.exit(1);
}
