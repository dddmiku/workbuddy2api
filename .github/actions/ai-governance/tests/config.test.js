// ═══ 更新日志 ═══
// 2026-09-18：验证非 GitHub AI 端点不能回退使用仓库凭据，同时保留独立密钥及官方模型端点配置。
const core = require('@actions/core');
const baseConfig = require('../config.json');
const { applyLocale, normalizeLanguage, parseInputs } = require('../src/utils/config');

function cloneConfig() {
  return JSON.parse(JSON.stringify(baseConfig));
}

describe('configuration', () => {
  test('uses chat completions by default', () => {
    expect(baseConfig.defaults.ai_api_type).toBe('chat-completions');
  });

  test('defaults unknown languages to English', () => {
    expect(normalizeLanguage('fr')).toBe('en');
    expect(applyLocale(cloneConfig(), 'fr')).toBe('en');
  });

  test('loads English responses by default', () => {
    const config = cloneConfig();
    applyLocale(config, 'en');

    expect(config.responses.issue_spam).toContain('This issue');
    expect(config.locale.answer_language).toBe('English');
  });

  test('loads Simplified Chinese when requested', () => {
    const config = cloneConfig();
    applyLocale(config, 'zh-CN');

    expect(config.responses.issue_spam).toContain('此Issue');
    expect(config.locale.answer_language).toBe('Simplified Chinese');
  });
});

describe('AI credential isolation', () => {
  const environmentKeys = ['INPUT_AI_BASE_URL', 'INPUT_AI_API_KEY'];
  let savedEnvironment;

  beforeEach(() => {
    savedEnvironment = Object.fromEntries(environmentKeys.map(key => [key, process.env[key]]));
    environmentKeys.forEach(key => { delete process.env[key]; });
  });

  afterEach(() => {
    jest.restoreAllMocks();
    environmentKeys.forEach(key => {
      if (savedEnvironment[key] === undefined) delete process.env[key];
      else process.env[key] = savedEnvironment[key];
    });
  });

  function parseEndpoint(base, independentKey = '', config = cloneConfig()) {
    const inputs = { 'github-token': 'fixture-github-credential', 'ai-base-url': base, 'ai-api-key': independentKey };
    jest.spyOn(core, 'getInput').mockImplementation(name => inputs[name] || '');
    return parseInputs(config);
  }

  test.each([
    'https://foreign.example.invalid/v1',
    'https://models.github.ai.foreign.example.invalid/v1',
    'https://models.github.ai@foreign.example.invalid/v1',
    'http://models.github.ai/inference'
  ])('rejects a missing independent key for %s', endpoint => {
    expect(() => parseEndpoint(endpoint)).toThrow(/ai-api-key/i);
  });

  test('also validates a configured default endpoint', () => {
    const config = cloneConfig();
    config.defaults.api_base_url = 'https://foreign.example.invalid/v1';
    expect(() => parseEndpoint('', '', config)).toThrow(/ai-api-key/i);
  });

  test('accepts an explicit key for a custom endpoint', () => {
    expect(parseEndpoint('https://foreign.example.invalid/v1', 'fixture-provider-credential').customApiKey).toBe('fixture-provider-credential');
  });

  test.each(['', 'https://models.github.ai/inference', 'https://models.inference.ai.azure.com'])('retains GitHub Models credential use for %s', endpoint => {
    expect(() => parseEndpoint(endpoint)).not.toThrow();
  });
});
