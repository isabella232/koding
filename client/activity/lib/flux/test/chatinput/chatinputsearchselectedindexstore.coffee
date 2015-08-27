{ expect } = require 'chai'

Reactor = require 'app/flux/reactor'

ChatInputSearchSelectedIndexStore = require 'activity/flux/stores/chatinput/chatinputsearchselectedindexstore'
actions = require 'activity/flux/actions/actiontypes'

describe 'ChatInputSearchSelectedIndexStore', ->

  beforeEach ->

    @reactor = new Reactor
    @reactor.registerStores chatInputSearchSelectedIndex : ChatInputSearchSelectedIndexStore


  describe '#setIndex', ->

    it 'sets selected index', ->

      index = 3

      @reactor.dispatch actions.SET_CHAT_INPUT_SEARCH_SELECTED_INDEX, { index }
      selectedIndex = @reactor.evaluate ['chatInputSearchSelectedIndex']

      expect(selectedIndex).to.equal index


  describe '#moveToNextIndex', ->

  	it 'moves to next index', ->

      index = 3
      nextIndex = index + 1

      @reactor.dispatch actions.SET_CHAT_INPUT_SEARCH_SELECTED_INDEX, { index }
      selectedIndex = @reactor.evaluate ['chatInputSearchSelectedIndex']

      expect(selectedIndex).to.equal index
      
      @reactor.dispatch actions.MOVE_TO_NEXT_CHAT_INPUT_SEARCH_INDEX
      selectedIndex = @reactor.evaluate ['chatInputSearchSelectedIndex']

      expect(selectedIndex).to.equal nextIndex


  describe '#moveToPrevIndex', ->

    it 'moves to prev index', ->

      index = 3
      prevIndex = index - 1

      @reactor.dispatch actions.SET_CHAT_INPUT_SEARCH_SELECTED_INDEX, { index }
      selectedIndex = @reactor.evaluate ['chatInputSearchSelectedIndex']

      expect(selectedIndex).to.equal index
      
      @reactor.dispatch actions.MOVE_TO_PREV_CHAT_INPUT_SEARCH_INDEX
      selectedIndex = @reactor.evaluate ['chatInputSearchSelectedIndex']

      expect(selectedIndex).to.equal prevIndex


  describe '#resetIndex', ->

    it 'resets selected index', ->

      index = 3

      @reactor.dispatch actions.SET_CHAT_INPUT_SEARCH_SELECTED_INDEX, { index }
      selectedIndex = @reactor.evaluate ['chatInputSearchSelectedIndex']

      expect(selectedIndex).to.equal index

      @reactor.dispatch actions.RESET_CHAT_INPUT_SEARCH_SELECTED_INDEX
      selectedIndex = @reactor.evaluate ['chatInputSearchSelectedIndex']

      expect(selectedIndex).to.equal 0

