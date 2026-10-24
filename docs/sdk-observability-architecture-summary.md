# Архитектура SDK и OpenTelemetry — резюме

_Рабочее резюме на 24 октября 2026 года. Документ предназначен для продолжения обсуждения в другой ветке._

## 1. Архитектура SDK

SDK защищает и открывает RPMSG/CFB-контейнеры. API строится в стиле **data-driven**: консьюмер сам составляет workflow из независимых функций, а SDK не владеет всей операцией обработки сообщения.

Чтение защищённого сообщения:

```text
MailCodec.Decompress
→ OpenContainer
→ RemoveProtection
→ MailCodec.ReadBody
→ ListAttachments / ReadAttachment
```

Создание защищённого сообщения:

```text
MailCodec.WriteBody / WriteAttachment
→ GetTemplates
→ SetProtectionFromTemplateId (или rights/license)
→ MailCodec.Compress
```

Основные части:

- `ProtectionEngine`: HTTP, получение лицензий, шифрование/дешифрование, кодирование контейнера;
- `MailCodec`: stateless-операции RPMSG/zlib/CFB;
- переданные консьюмером HTTP client и logger;
- read-only data objects: `ProtectionTemplate`, `ProtectionDescriptor`, `ProtectedContainerInfo` и другие.

Следствие data-driven дизайна: только консьюмер знает, что несколько независимых вызовов относятся к обработке одного сообщения.

## 2. Что хотим получать через OpenTelemetry

### Метрики

Нужно знать:

- сколько раз вызвали каждую операцию;
- сколько завершилось успешно, с ошибкой или отменой;
- error rate;
- p50/p95/p99 длительности;
- при необходимости — число одновременно выполняющихся операций.

Предлагаемые метрики:

```text
protection_sdk.operation.started
protection_sdk.operation.completed
protection_sdk.operation.duration
protection_sdk.operation.active       # опционально
```

Атрибуты должны иметь ограниченный набор значений:

```text
operation.name   = get_templates | remove_protection | mail_decompress | ...
operation.result = success | error | canceled
error.type       = network | unauthorized | invalid_license | io | ...
```

Нельзя помещать в metric attributes access token, filename, содержимое лицензии, `err.Error()` или произвольный `userID`: это секреты и/или высокая cardinality.

`started` и `completed` нужны отдельно: зависшая или оборванная операция попадёт в `started`, но не в `completed`.

### Traces и spans

Trace отвечает на вопрос: что происходило в одной конкретной операции и где было потрачено время. Trace состоит из spans с общим `trace_id`.

Базовая рекомендация:

- создавать spans для сетевых и составных операций `ProtectionEngine`;
- использовать автоматические HTTP spans;
- не создавать span на каждый AES/zlib-блок, getter или простой constructor;
- spans локальных стадий `MailCodec` считать опциональными, чтобы не портить основной API.

Message-level span создаёт консьюмер:

```text
message.process
├── mail.decompress
├── container.open
├── protection.remove
│   └── HTTP get_license
└── mail.read_body
```

Одинаковый `trace_id` означает одну операцию. `span_id` определяет конкретную стадию. Порядок sibling-spans виден по времени начала и длительности.

### Логи

- SDK пишет структурированные логи через logger консьюмера.
- Timestamp добавляет logger/exporter, а не SDK вручную.
- Один logger может одновременно писать в console и OTel sink; два разных logger API не нужны.
- Лог, записанный при активном span, получает `trace_id`/`span_id` и находится по сообщению или стадии.
- Нельзя логировать токены, ключи, license body, plaintext и другие чувствительные данные.

## 3. Кто чем владеет в OTel

```text
SDK instrumentation
→ provider / processor / reader приложения
→ exporter внутри приложения
→ опциональный OTel Collector
→ observability backend
```

- **Exporter** кодирует telemetry (обычно OTLP protobuf) и отправляет её из процесса.
- **Collector** опционален. Его pipeline: `receiver → processors → exporters`.
- Collector может фильтровать, batch-ить, обогащать и отправлять данные в несколько backends.
- Destination может быть backend разработчика SDK, backend консьюмера или оба сразу.
- Запись counter или завершение span обычно не делает отдельный сетевой запрос: отправка выполняется batch-ами.
- Кто создал providers/exporters, тот вызывает flush/shutdown.
- Экспортер непосредственно в SDK передавать не нужно.

SDK создаёт telemetry, но приложение задаёт endpoint, credentials, exporter и sampling. Если данные должны идти в backend разработчика SDK, консьюмер настраивает соответствующий OTLP endpoint либо fan-out в Collector.

## 4. Поддержка платформ

Консервативная оценка:

| Платформа | Traces/spans | Metrics | Logs | Модель контекста |
|---|---|---|---|---|
| Go | Stable | Stable | Release candidate | Явный `context.Context` |
| Swift | Stable | Development | Development | Active/task-local context |
| .NET | Stable | Stable | Stable | Ambient `Activity.Current`; отмена отдельно |

Примечания:

- Официальная страница Swift пока помечает metrics/logs как Development, хотя новый `opentelemetry-swift-core` называет все три сигнала stable. Пока документация не согласована, считаем Swift metrics/logs нестабильными.
- Span не является отдельным четвёртым сигналом: spans образуют trace.

Источники статусов:

- <https://opentelemetry.io/docs/languages/go/>
- <https://opentelemetry.io/docs/languages/swift/>
- <https://opentelemetry.io/docs/languages/dotnet/>

## 5. Go

### Context policy

`context.Context` нельзя сохранять в `ProtectionEngine`: он относится к одному вызову.

Рекомендуемый core API:

- HTTP/cancellable методы получают `ctx` первым параметром;
- локальные stateless-операции остаются без `ctx`, если SDK не обещает для них связанные spans;
- метрики сами по себе не требуют `ctx` в публичном API;
- наличие `ctx` не делает AES/zlib автоматически отменяемыми: нужны проверки между блоками.

```go
func (e *Engine) RemoveProtection(
    ctx context.Context,
    src io.ReadSeeker,
    info *ProtectedContainerInfo,
    dst io.Writer,
    serviceURL string,
    accessToken string,
) error

func (c *MailCodec) Decompress(
    src io.Reader,
    dst io.Writer,
) error
```

Если консьюмер строит pipeline через goroutines/channels и хочет единый trace, context переносится вместе с конкретным сообщением:

```go
type messageJob struct {
    ctx  context.Context
    data MessageData
}
```

Это correlation contract: каждый message job должен иметь собственный operation context. Если такой контракт нежелателен в core API, tracing всего workflow остаётся ответственностью консьюмера либо отдельного OTel adapter package.

### Providers и concurrency

- Консьюмер создаёт `TracerProvider`/`MeterProvider`, настраивает exporters и владеет shutdown.
- При explicit design providers передаются SDK, а SDK сохраняет полученные `Tracer`, `Counter`, `Histogram`.
- Global OTel providers проще, но создают глобальное состояние.
- OTel providers, tracers, spans, meters и instruments concurrency-safe; внешний mutex не нужен.
- Instruments создаются один раз, не на каждый вызов.

## 6. Swift

В Swift активный trace context обычно task-local, поэтому отдельный telemetry-context параметр публичным методам не нужен. Отмена реализуется средствами Swift concurrency.

Рекомендуемые нейтральные API Swift Server ecosystem:

- `swift-log`;
- `swift-metrics`;
- `swift-distributed-tracing` / `ServiceContext`;
- OTel подключается приложением как backend/exporter.

В structured concurrency контекст следует за Task. `Task.detached` и долгоживущие workers, читающие channel/stream, не получают per-message context автоматически: его нужно переносить вместе с work item.

## 7. .NET

SDK использует платформенные API, а не OTel SDK напрямую:

- `System.Diagnostics.ActivitySource` для traces;
- `System.Diagnostics.Metrics.Meter` для metrics;
- `Microsoft.Extensions.Logging.ILogger` для logs.

SDK публикует стабильные имена:

```csharp
public const string ActivitySourceName = "Company.ProtectionSdk";
public const string MeterName = "Company.ProtectionSdk";
```

Консьюмер подписывает OTel на эти источники:

```csharp
builder.Services.AddOpenTelemetry()
    .WithTracing(t => t
        .AddSource(ProtectionTelemetry.ActivitySourceName)
        .AddHttpClientInstrumentation()
        .AddOtlpExporter())
    .WithMetrics(m => m
        .AddMeter(ProtectionTelemetry.MeterName)
        .AddHttpClientInstrumentation()
        .AddOtlpExporter());

builder.Logging.AddOpenTelemetry(l =>
{
    l.IncludeFormattedMessage = true;
    l.IncludeScopes = true;
    l.AddOtlpExporter();
});
```

Providers в `ProtectionEngine` не передаются. OTel provider слушает именованные `ActivitySource` и `Meter`. Без listener instrumentation практически неактивна.

`Activity.Current` автоматически проходит через обычный `async/await`. `CancellationToken` передаётся отдельно только cancellable-методам.

Через долгоживущий `Channel<T>` ambient context вместе с элементом автоматически не переносится. Work item должен содержать `ActivityContext`, а worker создаёт stage activity с ним как parent. Логи через `ILogger` получают `TraceId`/`SpanId` текущей activity.

## 8. Logger и HTTP client

- Logger создаёт и передаёт консьюмер; должен существовать no-op вариант.
- Logger и HTTP client должны быть safe для concurrent calls.
- В .NET предпочтителен `ILogger` либо adapter из общего `Logger` в `ILogger`.
- В Go коррелированный `slog` требует operation context. Интерфейс `Logger.Log(...)` без context не сможет автоматически получить текущий span.
- HTTP client создаёт и переиспользует консьюмер.
- Нужно избегать двойной HTTP instrumentation, если transport уже обёрнут консьюмером.

## 9. Разделение ответственности

| Ответственность | SDK | Консьюмер |
|---|---:|---:|
| Имена SDK metrics/spans | Да | Нет |
| Запись SDK metrics/spans | Да | Нет |
| Определение полного message workflow | Нет | Да |
| Создание message-level span | Нет | Да |
| Endpoint/API key/exporter/sampling | Нет | Да |
| Collector fan-out | Нет | Да/инфраструктура |
| Logger и HTTP client | Нет | Да |
| Flush/shutdown telemetry | Нет | Да |
| Защита от secrets/high cardinality | Да | Да |

## 10. Открытые решения

1. **Go: providers или globals.** Explicit provider injection лучше тестируется; globals упрощают подключение.
2. **Локальные stage spans.** Если `MailCodec` должен сам создавать связанные spans в Go, потребуется `context.Context` или отдельный context-aware adapter.
3. **Swift implementation.** Direct `opentelemetry-swift` или нейтральные Swift Server APIs с OTel backend.
4. **Destination policy.** Явно определить opt-in/обязательность, endpoint, authentication и privacy для backend разработчика SDK.
5. **Error taxonomy.** Зафиксировать общий ограниченный набор `error.type` для Go/Swift/.NET.
6. **Semantic names.** Зафиксировать instrumentation scope, metric names, span names, units и versioning до релиза.

## 11. Главный вывод

Метрики можно встроить в SDK почти без влияния на API. Traces отдельных вызовов также просты. Нельзя автоматически определить, что независимые data-driven вызовы через goroutines, tasks или channels принадлежат одной операции сообщения. Эту границу задаёт консьюмер и переносит correlation state: `context.Context` в Go, `Activity.Current`/`ActivityContext` в .NET и task-local/`ServiceContext` в Swift.
