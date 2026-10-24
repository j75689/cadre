// The name the UI acts as. A placeholder until OIDC: the user types it once
// and it is kept in this browser; it becomes the author of their actions, as
// --as is for the CLI. Nothing checks it.

const key = "cadre.identity";

export function loadIdentity(): string {
  try {
    return localStorage.getItem(key) ?? "";
  } catch {
    return "";
  }
}

export function saveIdentity(name: string): void {
  try {
    localStorage.setItem(key, name);
  } catch {
    // storage unavailable: the name lasts this page only
  }
}
