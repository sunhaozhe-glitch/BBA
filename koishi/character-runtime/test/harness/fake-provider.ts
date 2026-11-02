import { AIMessage } from '@langchain/core/messages'
import { FakeStreamingChatModel } from '@langchain/core/utils/testing'
import type { Context } from '../../src/koishi.js'
import { ChatLunaPlugin } from 'koishi-plugin-chatluna/services/chat'
import { PlatformModelClient } from 'koishi-plugin-chatluna/llm-core/platform/client'
import type { ChatLunaChatModel } from 'koishi-plugin-chatluna/llm-core/platform/model'
import { ModelCapabilities, ModelType } from 'koishi-plugin-chatluna/llm-core/platform/types'
import type { BaseMessage } from '@langchain/core/messages'

/**
 * FIXTURE ONLY - a ChatLuna platform client that returns deterministic text and
 * never opens a socket. It exercises the real ChatLuna service and the real
 * Character plugin while replacing the model provider.
 */
export interface FakeProviderOptions {
  platform?: string
  model?: string
  replies?: string[]
  onGenerate?: (messages: BaseMessage[]) => void
}

class FakeModelClient extends PlatformModelClient {
  readonly platform: string

  constructor(
    ctx: Context,
    configPool: unknown,
    platform: string,
    private readonly modelName: string,
    private readonly replies: string[],
    private readonly onGenerate?: FakeProviderOptions['onGenerate']
  ) {
    super(ctx, configPool as never)
    this.platform = platform
  }

  async refreshModels(): Promise<
    Array<{
      name: string
      type: ModelType
      maxTokens: number
      capabilities: ModelCapabilities[]
    }>
  > {
    return [
      {
        name: this.modelName,
        type: ModelType.llm,
        maxTokens: 8192,
        capabilities: [ModelCapabilities.TextInput]
      }
    ]
  }

  protected _createModel(): ChatLunaChatModel {
    const model = new FakeStreamingChatModel({
      responses: this.replies.map((reply) => new AIMessage(reply)),
      sleep: 0,
      toolStyle: 'openai'
    })
    model.getNumTokens = async (content) => Math.ceil(String(content).length / 4)
    const generate = model._generate.bind(model)
    model._generate = async (messages, options, runManager) => {
      this.onGenerate?.(messages)
      return generate(messages, options, runManager)
    }
    return model as unknown as ChatLunaChatModel
  }
}

export interface FakeProviderPlugin {
  name: string
  inject: string[]
  apply(ctx: Context): void
}

export function createFakeProviderPlugin(
  options: FakeProviderOptions = {}
): FakeProviderPlugin {
  const platform = options.platform ?? 'fake'
  const model = options.model ?? 'fake-model'
  const replies = options.replies ?? ['<message>离线测试回复</message>']

  return {
    name: 'fake-chatluna-provider',
    // Upstream platform adapters run only once the real ChatLuna service (and
    // its platform registry) exists; accessing it earlier kills the App.
    inject: ['chatluna'],
    apply(ctx: Context): void {
      const plugin = new ChatLunaPlugin(
        ctx,
        {
          chatConcurrentMaxSize: 1,
          chatTimeLimit: 100,
          configMode: 'default',
          maxRetries: 0,
          timeout: 10_000,
          proxyMode: 'off'
        } as never,
        platform,
        true
      )
      plugin.parseConfig(() => [
        {
          apiKey: 'offline-fixture',
          platform,
          maxRetries: 0,
          concurrentMaxSize: 1,
          timeout: 10_000,
          chatLimit: 100
        } as never
      ])
      plugin.registerClient(
        () => new FakeModelClient(ctx, plugin.platformConfigPool, platform, model, replies, options.onGenerate)
      )
      // Same upstream lifecycle as a real platform adapter: install the
      // platform on ready and materialize its model list through ChatLuna.
      ctx.on('ready', () => plugin.initClient())
    }
  }
}
