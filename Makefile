.PHONY: lint
lint:
	$(MAKE) -C orlop lint; orlop_rc=$$?; \
	$(MAKE) -C platform-api lint; api_rc=$$?; \
	exit $$(( orlop_rc > api_rc ? orlop_rc : api_rc ))

.PHONY: lint-fix
lint-fix:
	$(MAKE) -C orlop lint-fix
	$(MAKE) -C platform-api lint-fix

.PHONY: lint-fmt
lint-fmt:
	$(MAKE) -C orlop lint-fmt
	$(MAKE) -C platform-api lint-fmt

.PHONY: test
test:
	$(MAKE) -C orlop test
	$(MAKE) -C platform-api test
	$(MAKE) -C controllers test
