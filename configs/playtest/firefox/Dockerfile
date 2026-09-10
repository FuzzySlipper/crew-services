# Reuse the existing maintained Firefox image, including its current browser.
ARG FIREFOX_IMAGE=den-playtest-firefox:local
FROM ${FIREFOX_IMAGE}
COPY --chmod=755 playtest-forward /opt/gow/playtest-forward
COPY --chmod=755 startup-app.sh /opt/gow/startup-app.sh
ENV RUN_GAMESCOPE=1 MOZ_ENABLE_WAYLAND=0 PLAYTEST_BROWSER_STARTUP=1
