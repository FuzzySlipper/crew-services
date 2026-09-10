// Dedicated agent browser: do not interrupt sessions with first-run UI.
user_pref("termsofuse.bypassNotification", true);
user_pref("browser.aboutwelcome.enabled", false);
user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("browser.sessionstore.resume_from_crash", false);
user_pref("browser.startup.couldRestoreSession.count", -1);
user_pref("datareporting.policy.dataSubmissionPolicyBypassNotification", true);
// Gamescope/XWayland: parent-process recentering adds the pre-lock cursor
// offset to the first gameplay delta. The older recenter path preserves exact
// native deltas, including repeated lock/unlock and movement past screen edges.
user_pref("dom.pointer-lock.reset-to-center-from-parent.enabled", false);
