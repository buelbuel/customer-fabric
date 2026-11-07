// Invoked only through the local Docker operator bootstrap command.
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
