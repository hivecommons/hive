    async function prepareIntegrationExecutionIdentity() {
      const target = document.getElementById('integration-execution-identity');
      if (!target) return;
      target.textContent = 'Preparing create-only local identity; no runtime configuration or forge mutation…';
      try {
        const response = await fetch('/api/integration-orchestration/execution-identity', {
          method: 'POST', headers: _inceptionAuthHeaders()
        });
        if (!response.ok) throw new Error(await saveErrorMessage(response));
        target.textContent = JSON.stringify(await response.json(), null, 2);
      } catch (error) {
        target.textContent = 'Identity unavailable: ' + String(error.message || error);
      }
    }
