export { startCollector, stableMessageRef, stop, stopCollector } from './runtime.js'
export { CollectorBoundaryError, CollectorError, isCollectorBoundaryError, isCollectorError } from './errors.js'
export { buildCapabilityMatrix, buildLimitations } from './config.js'
export { PINNED_VERSIONS } from './types.js'
export { isOutboundDenied } from './boundary.js'
export {
  DEFAULT_SUMMARY_RETENTION_DAYS,
  createDeterministicSummaryProducer,
  createFixtureSummaryModelAdapter,
  createHybridSummaryProducer,
  createProviderSummaryModelAdapter,
  createSemanticSummaryProducer,
  runSummaryAnalysisJob
} from './summary-producer.js'
export {
  SEMANTIC_ANALYSIS_SCHEMA_ID,
  SEMANTIC_BOUNDS,
  assertSemanticPrivacy,
  createFixtureSemanticAnalyzer,
  createProviderSemanticAnalyzer,
  observedActivityFor,
  semanticLimitations,
  semanticTopics,
  summaryExpiry,
  validateSemanticAnalysis
} from './semantic-analysis.js'
export type {
  SemanticActivityWindow,
  SemanticAnalysis,
  SemanticAnalysisInput,
  SemanticAnalyzer,
  SemanticBasis,
  SemanticClaim,
  SemanticClaimKind,
  SemanticEvidenceSource,
  SemanticMessage
} from './semantic-analysis.js'
export { DEFAULT_SUMMARY_PUBLISH_PATH, SummaryBackendPublisher } from './summary-publisher.js'
export type * from './types.js'
export type {
  ProducedTopic,
  SummaryAnalysisJobOptions,
  SummaryAnalysisResult,
  SummaryEvidenceKind,
  SummaryModelAdapter,
  SummaryModelMessage,
  SummaryModelRequest,
  SummaryProducer,
  SummaryProducerInput,
  SummaryProduction
} from './summary-producer.js'
export type {
  SummaryBackendPublisherOptions,
  SummaryPublishInput,
  SummaryPublishResult,
  SummaryPublisher
} from './summary-publisher.js'
export type { ResolvedCollectorOptions } from './config.js'
